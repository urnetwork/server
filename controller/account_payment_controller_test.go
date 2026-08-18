package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"maps"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

var sendPaymentTransactionId = "123456"

func TestSendPaymentsUsesBoundedPlannerAndPreservesPartialError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		laterSliceErr := errors.New("later slice failed")
		called := false
		err := sendPaymentsWithPlanner(clientSession, func(
			plannerCtx context.Context,
			maxDuration time.Duration,
			onSlice func(*model.PaymentPlan),
		) ([]*model.PaymentPlan, error) {
			called = true
			if plannerCtx != clientSession.Ctx {
				t.Fatal("planner did not receive the client session context")
			}
			wantMaxDuration := boundedPaymentPlanSliceDuration(model.EnvSubsidyConfig().MinDurationPerPayout())
			if maxDuration != wantMaxDuration {
				t.Fatalf("planner maxDuration = %s, want %s", maxDuration, wantMaxDuration)
			}
			if onSlice != nil {
				t.Fatal("send path unexpectedly supplied an onSlice callback")
			}
			// The first slice is already durable even though a later slice failed.
			return []*model.PaymentPlan{{
				PaymentPlanId:   server.NewId(),
				NetworkPayments: map[server.Id]*model.AccountPayment{},
			}}, laterSliceErr
		})

		if !called {
			t.Fatal("bounded planner was not called")
		}
		if !errors.Is(err, laterSliceErr) {
			t.Fatalf("send error = %v, want %v", err, laterSliceErr)
		}
	})
}

func TestBoundedPaymentPlanSliceDurationExceedsMinimumSubsidyDuration(t *testing.T) {
	tests := []struct {
		name                string
		minSubsidyDuration  time.Duration
		wantPaymentDuration time.Duration
	}{
		{
			name:                "normal production minimum",
			minSubsidyDuration:  3 * 24 * time.Hour,
			wantPaymentDuration: 4 * 24 * time.Hour,
		},
		{
			name:                "configured minimum exceeds preferred slice",
			minSubsidyDuration:  10 * 24 * time.Hour,
			wantPaymentDuration: 11 * 24 * time.Hour,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := boundedPaymentPlanSliceDuration(test.minSubsidyDuration)
			if got != test.wantPaymentDuration {
				t.Fatalf("slice duration = %s, want %s", got, test.wantPaymentDuration)
			}
			if got <= test.minSubsidyDuration {
				t.Fatalf("slice duration %s does not exceed minimum subsidy duration %s", got, test.minSubsidyDuration)
			}
		})
	}
}

func TestCollectMissingWalletNoticesDeduplicatesAcrossSlices(t *testing.T) {
	networkId := server.NewId()
	firstPaymentId := server.NewId()
	walletNetworkId := server.NewId()
	walletId := server.NewId()

	notices := collectMissingWalletNotices([]*model.PaymentPlan{
		{
			NetworkPayments: map[server.Id]*model.AccountPayment{
				networkId: {
					PaymentId: firstPaymentId,
					Payout:    125,
				},
				walletNetworkId: {
					PaymentId: server.NewId(),
					WalletId:  &walletId,
					Payout:    999,
				},
			},
		},
		{
			NetworkPayments: map[server.Id]*model.AccountPayment{
				networkId: {
					PaymentId: server.NewId(),
					Payout:    375,
				},
			},
		},
	})

	if len(notices) != 1 {
		t.Fatalf("missing-wallet notices = %v, want one network", notices)
	}
	notice := notices[networkId]
	if notice == nil {
		t.Fatalf("missing-wallet notice for %s was not collected", networkId)
	}
	if notice.paymentId != firstPaymentId {
		t.Fatalf("notice payment id = %s, want first slice id %s", notice.paymentId, firstPaymentId)
	}
	if notice.payout != 500 {
		t.Fatalf("notice payout = %d, want 500", notice.payout)
	}
}

func TestSubscriptionSendPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

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
		connect.AssertEqual(t, err, nil)
		model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceSession.ByJwt.NetworkId,
		}, ctx)

		transferEscrow, err := model.CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024*1024)
		connect.AssertEqual(t, err, nil)

		usedTransferByteCount := model.ByteCount(1024)
		paidByteCount := usedTransferByteCount

		model.CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		model.CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)

		paid := model.UsdToNanoCents(model.ProviderRevenueShare * model.NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		destinationWalletAddress := "0x1234567890"

		args := &model.CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    destinationWalletAddress,
			DefaultTokenType: "USDC",
		}
		walletId := model.CreateAccountWalletExternal(destinationSession, args)
		connect.AssertNotEqual(t, walletId, nil)

		wallet := model.GetAccountWallet(ctx, *walletId)

		err = model.SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)
		connect.AssertEqual(t, err, nil)

		paymentPlan, err := model.PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(paymentPlan.NetworkPayments), 0)
		connect.AssertEqual(t, paymentPlan.WithheldNetworkIds, []server.Id{destinationNetworkId})

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
			connect.AssertEqual(t, err, nil)

			err = model.CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = model.CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paidByteCount += usedTransferByteCount
			paid += model.UsdToNanoCents(model.ProviderRevenueShare * model.NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		contractIds := model.GetOpenContractIds(ctx, sourceId, destinationId)
		connect.AssertEqual(t, len(contractIds), 0)

		paymentPlan, err = model.PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, slices.Collect(maps.Keys(paymentPlan.NetworkPayments)), []server.Id{destinationNetworkId})

		// these should hit -> default
		// payment.PaymentRecord should all be empty
		// meaning they will make a call to send a transaction to the provider
		for _, payment := range paymentPlan.NetworkPayments {

			// initiate payment
			complete, canceled, err := advancePayment(payment, destinationSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, complete, false)
			connect.AssertEqual(t, canceled, false)

			// fetch the payment record which should have some updated fields
			paymentRecord, err := model.GetPayment(ctx, payment.PaymentId)
			connect.AssertEqual(t, err, nil)

			// check the wallet address is correctly joined
			connect.AssertEqual(t, paymentRecord.WalletAddress, destinationWalletAddress)

			// estimate fee
			// estimatedFees, err := mockCircleClient.EstimateTransferFee(
			// 	ctx,
			// 	*paymentRecord.TokenAmount,
			// 	wallet.WalletAddress,
			// 	"MATIC",
			// )
			// connect.AssertEqual(t, err, nil)

			// // deduct fee from payment amount
			// fee, err := CalculateFee(
			// 	*estimatedFees.Medium,
			// 	"MATIC",
			// )
			// connect.AssertEqual(t, err, nil)

			// // convert fee to usdc
			// feeInUSDC, err := ConvertFeeToUSDC(ctx, "MATIC", *fee)
			feeInUSDC := 0.01 // this is now hardcoded due to Circle API errors

			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, paymentRecord.PaymentId, payment.PaymentId)
			connect.AssertEqual(t, paymentRecord.Blockchain, "MATIC")

			// payoutAmount (in USD) - fee (in USD) = token amount
			connect.AssertEqual(t, model.NanoCentsToUsd(paymentRecord.Payout)-feeInUSDC, float64(*paymentRecord.TokenAmount))
			connect.AssertEqual(t, paymentRecord.PaymentRecord, sendPaymentTransactionId)
		}

		pendingPayments := model.GetPendingPaymentsInPlan(ctx, paymentPlan.PaymentPlanId)

		// coinbase api will return a pending status
		// should return that payment is incomplete
		for _, payment := range pendingPayments {
			complete, canceled, err := advancePayment(payment, destinationSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, complete, false)
			connect.AssertEqual(t, canceled, false)

			// account balance should not yet be updated
			accountBalance := model.GetAccountBalance(destinationSession)
			connect.AssertEqual(t, int64(0), accountBalance.Balance.PaidByteCount)
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
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, complete, true)
			connect.AssertEqual(t, canceled, false)

			paymentRecord, err := model.GetPayment(ctx, payment.PaymentId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, paymentRecord.Completed, true)
			connect.AssertEqual(t, paymentRecord.TxHash, mockTxHash)
			connect.AssertEqual(t, paymentRecord.Blockchain, "MATIC")

			// check that the account balance has been updated
			accountBalance := model.GetAccountBalance(destinationSession)
			connect.AssertEqual(t, accountBalance.Balance.PaidByteCount, paidByteCount)
		}

		networkPayments, err := GetNetworkAccountPayments(destinationSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(networkPayments.AccountPayments), len(pendingPayments))
		connect.AssertEqual(t, networkPayments.AccountPayments[0].NetworkId, destinationSession.ByJwt.NetworkId)

		sourcePayments, err := GetNetworkAccountPayments(sourceSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(sourcePayments.AccountPayments), 0)
	})
}

func TestAdvancePaymentWalletSafetyAndIdempotency(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := model.ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := model.UsdToNanoCents(10.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		type capturedSend struct {
			idempotencyKey server.Id
			address        string
		}
		sends := []capturedSend{}
		sendErr := error(nil)

		mockCircleClient := &mockCircleApiClient{
			GetTransactionFunc: defaultGetTransactionDataHandler,
			CreateTransferTransactionFunc: func(
				ctx context.Context,
				idempotencyKey server.Id,
				amount float64,
				destinationAddress string,
				network string,
			) (*CreateTransferTransactionResult, error) {
				sends = append(sends, capturedSend{
					idempotencyKey: idempotencyKey,
					address:        destinationAddress,
				})
				if sendErr != nil {
					return nil, sendErr
				}
				return &CreateTransferTransactionResult{
					Id:    sendPaymentTransactionId,
					State: "INITIATED",
				}, nil
			},
			EstimateTransferFeeFunc: defaultEstimateFeeHandler,
		}
		SetCircleClient(mockCircleClient)

		SetMessageSender(&mockAWSMessageSender{
			SendMessageFunc: func(userAuth string, template Template, sendOpts ...any) error {
				return nil
			},
		})

		balanceCode, err := model.CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, ctx)

		wallet1Address := "0x1111"
		wallet1Id := model.CreateAccountWalletExternal(destinationSession, &model.CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    wallet1Address,
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, wallet1Id, nil)
		err = model.SetPayoutWallet(ctx, destinationNetworkId, *wallet1Id)
		connect.AssertEqual(t, err, nil)

		// provide enough traffic to exceed the min payout
		usedTransferByteCount := model.ByteCount(1024 * 1024 * 1024)
		paid := model.NanoCents(0)
		for paid < model.UsdToNanoCents(model.EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := model.CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = model.CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = model.CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paid += model.UsdToNanoCents(model.ProviderRevenueShare * model.NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		paymentPlan, err := model.PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		payment, ok := paymentPlan.NetworkPayments[destinationNetworkId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, *payment.WalletId, *wallet1Id)

		advanceArgs := &AdvancePaymentArgs{
			PaymentId: payment.PaymentId,
		}

		// the network removes its payout wallet before the payment is sent.
		// funds must not be sent to the deactivated wallet.
		removeResult := model.RemoveWallet(*wallet1Id, destinationSession)
		connect.AssertEqual(t, removeResult.Success, true)

		result, err := AdvancePayment(advanceArgs, destinationSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Complete, false)
		connect.AssertEqual(t, result.Canceled, true)
		connect.AssertEqual(t, len(sends), 0)

		// the payment is held, not canceled, so it can pay out
		// after the network sets a valid wallet
		heldPayment, err := model.GetPayment(ctx, payment.PaymentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, heldPayment.Canceled, false)
		connect.AssertEqual(t, heldPayment.Completed, false)
		connect.AssertEqual(t, heldPayment.PaymentRecord, nil)

		// the network sets a new payout wallet
		wallet2Address := "0x2222"
		wallet2Id := model.CreateAccountWalletExternal(destinationSession, &model.CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    wallet2Address,
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, wallet2Id, nil)
		err = model.SetPayoutWallet(ctx, destinationNetworkId, *wallet2Id)
		connect.AssertEqual(t, err, nil)

		// the first submit fails after the processor call.
		// the same idempotency key must be reused on the retry,
		// so the processor cannot double-send the funds.
		sendErr = fmt.Errorf("simulated processor error")
		result, err = AdvancePayment(advanceArgs, destinationSession)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, len(sends), 1)
		connect.AssertEqual(t, sends[0].address, wallet2Address)

		sendErr = nil
		result, err = AdvancePayment(advanceArgs, destinationSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Complete, false)
		connect.AssertEqual(t, result.Canceled, false)
		connect.AssertEqual(t, len(sends), 2)
		connect.AssertEqual(t, sends[1].idempotencyKey, sends[0].idempotencyKey)
		connect.AssertEqual(t, sends[1].address, wallet2Address)

		sentPayment, err := model.GetPayment(ctx, payment.PaymentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *sentPayment.WalletId, *wallet2Id)
		connect.AssertEqual(t, *sentPayment.WalletAddress, wallet2Address)
		connect.AssertEqual(t, *sentPayment.PaymentRecord, sendPaymentTransactionId)

		// a failed transaction resets the record and gets a fresh
		// idempotency key, so a new transaction can be created
		mockCircleClient.GetTransactionFunc = func(ctx context.Context, transactionId string) (*GetTransactionResult, error) {
			return &GetTransactionResult{
				Transaction: CircleTransaction{
					State:      "FAILED",
					Id:         transactionId,
					Blockchain: "MATIC",
				},
				ResponseBodyBytes: []byte(`{"state":"FAILED"}`),
			}, nil
		}
		result, err = AdvancePayment(advanceArgs, destinationSession)
		connect.AssertNotEqual(t, err, nil)

		mockCircleClient.GetTransactionFunc = defaultGetTransactionDataHandler
		result, err = AdvancePayment(advanceArgs, destinationSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(sends), 3)
		connect.AssertNotEqual(t, sends[2].idempotencyKey, sends[0].idempotencyKey)
		connect.AssertEqual(t, sends[2].address, wallet2Address)

		// complete the payment
		mockTxHash := "0xabcdef"
		mockCircleClient.GetTransactionFunc = func(ctx context.Context, transactionId string) (*GetTransactionResult, error) {
			return &GetTransactionResult{
				Transaction: CircleTransaction{
					State:      "COMPLETE",
					Id:         transactionId,
					Blockchain: "MATIC",
					TxHash:     mockTxHash,
				},
				ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`),
			}, nil
		}
		result, err = AdvancePayment(advanceArgs, destinationSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Complete, true)

		completedPayment, err := model.GetPayment(ctx, payment.PaymentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, completedPayment.Completed, true)
		connect.AssertEqual(t, *completedPayment.WalletId, *wallet2Id)
		connect.AssertEqual(t, *completedPayment.WalletAddress, wallet2Address)

	})
}

// Circle retry states must remain pending regardless of age. CONFIRMED already
// has an on-chain hash, so persisting that hash makes the retry reconcilable;
// only a non-contradictory terminal CANCELLED state may clear retry markers and
// release a payment for replacement.
func TestAdvancePaymentRetryAndTerminalCancellationState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
		})
		defer clientSession.Cancel()

		insertRetryPayment := func(record string, txHash *string) *model.AccountPayment {
			paymentId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO account_payment (
						payment_id,
						payment_plan_id,
						network_id,
						wallet_id,
						payout_byte_count,
						payout_nano_cents,
						min_sweep_time,
						payment_record,
						circle_idempotency_key,
						tx_hash
					) VALUES ($1, $2, $3, NULL, 100, 100, $4, $5, $6, $7)
					`,
					paymentId,
					server.NewId(),
					networkId,
					server.NowUtc(),
					record,
					server.NewId(),
					txHash,
				))
			})
			payment, err := model.GetPayment(ctx, paymentId)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, payment, nil)
			return payment
		}
		paymentHasIdempotencyKey := func(paymentId server.Id) bool {
			var hasIdempotencyKey bool
			server.Db(ctx, func(conn server.PgConn) {
				result, queryErr := conn.Query(
					ctx,
					`SELECT circle_idempotency_key IS NOT NULL FROM account_payment WHERE payment_id = $1`,
					paymentId,
				)
				server.WithPgResult(result, queryErr, func() {
					if result.Next() {
						server.Raise(result.Scan(&hasIdempotencyKey))
					}
				})
			})
			return hasIdempotencyKey
		}

		{
			payment := insertRetryPayment("circle-confirmed", nil)
			const txHash = "confirmed-chain-hash"
			const receipt = `{"state":"CONFIRMED","txHash":"confirmed-chain-hash"}`
			SetCircleClient(&mockCircleApiClient{
				GetTransactionFunc: func(context.Context, string) (*GetTransactionResult, error) {
					return &GetTransactionResult{
						Transaction: CircleTransaction{
							State:  "CONFIRMED",
							TxHash: txHash,
						},
						ResponseBodyBytes: []byte(receipt),
					}, nil
				},
			})

			complete, canceled, err := advancePayment(payment, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, complete, false)
			connect.AssertEqual(t, canceled, false)

			updated, err := model.GetPayment(ctx, payment.PaymentId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, updated.Completed, false)
			connect.AssertEqual(t, updated.Canceled, false)
			connect.AssertNotEqual(t, updated.PaymentRecord, nil)
			connect.AssertNotEqual(t, updated.TxHash, nil)
			connect.AssertNotEqual(t, updated.PaymentReceipt, nil)
			connect.AssertEqual(t, *updated.PaymentRecord, "circle-confirmed")
			connect.AssertEqual(t, *updated.TxHash, txHash)
			connect.AssertEqual(t, *updated.PaymentReceipt, receipt)
		}

		{
			// Reproduce the old cancel/complete race with the stale payment object
			// already held by the worker. A rejected DB completion must not be
			// reported as complete, or this worker would silently stop retrying.
			payment := insertRetryPayment("circle-complete-race", nil)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE account_payment SET canceled = true, cancel_time = now() WHERE payment_id = $1`,
					payment.PaymentId,
				))
			})
			SetCircleClient(&mockCircleApiClient{
				GetTransactionFunc: func(context.Context, string) (*GetTransactionResult, error) {
					return &GetTransactionResult{
						Transaction: CircleTransaction{
							State:  "COMPLETE",
							TxHash: "complete-chain-hash",
						},
						ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`),
					}, nil
				},
			})

			complete, canceled, err := advancePayment(payment, clientSession)
			connect.AssertNotEqual(t, err, nil)
			connect.AssertEqual(t, complete, false)
			connect.AssertEqual(t, canceled, false)

			updated, err := model.GetPayment(ctx, payment.PaymentId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, updated.Completed, false)
			connect.AssertEqual(t, updated.Canceled, true)
			connect.AssertNotEqual(t, updated.PaymentRecord, nil)
		}

		{
			oldHash := "mempool-hash"
			payment := insertRetryPayment("circle-cancelled-after-sent", &oldHash)
			const receipt = `{"state":"CANCELLED"}`
			SetCircleClient(&mockCircleApiClient{
				GetTransactionFunc: func(context.Context, string) (*GetTransactionResult, error) {
					return &GetTransactionResult{
						Transaction:       CircleTransaction{State: "CANCELLED"},
						ResponseBodyBytes: []byte(receipt),
					}, nil
				},
			})

			complete, canceled, err := advancePayment(payment, clientSession)
			connect.AssertNotEqual(t, err, nil)
			connect.AssertEqual(t, complete, false)
			connect.AssertEqual(t, canceled, false)

			updated, err := model.GetPayment(ctx, payment.PaymentId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, updated.Canceled, false)
			connect.AssertNotEqual(t, updated.PaymentRecord, nil)
			connect.AssertNotEqual(t, updated.TxHash, nil)
			connect.AssertNotEqual(t, updated.PaymentReceipt, nil)
			connect.AssertEqual(t, *updated.PaymentRecord, "circle-cancelled-after-sent")
			connect.AssertEqual(t, *updated.TxHash, oldHash)
			connect.AssertEqual(t, *updated.PaymentReceipt, receipt)
			connect.AssertEqual(t, paymentHasIdempotencyKey(payment.PaymentId), true)
		}

		{
			payment := insertRetryPayment("circle-cancelled", nil)
			const receipt = `{"state":"CANCELLED"}`
			SetCircleClient(&mockCircleApiClient{
				GetTransactionFunc: func(context.Context, string) (*GetTransactionResult, error) {
					return &GetTransactionResult{
						Transaction:       CircleTransaction{State: "CANCELLED"},
						ResponseBodyBytes: []byte(receipt),
					}, nil
				},
			})

			complete, canceled, err := advancePayment(payment, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, complete, false)
			connect.AssertEqual(t, canceled, true)

			updated, err := model.GetPayment(ctx, payment.PaymentId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, updated.Canceled, true)
			connect.AssertEqual(t, updated.PaymentRecord, nil)
			connect.AssertEqual(t, updated.TxHash, nil)
			connect.AssertNotEqual(t, updated.PaymentReceipt, nil)
			connect.AssertEqual(t, *updated.PaymentReceipt, receipt)
			connect.AssertEqual(t, paymentHasIdempotencyKey(payment.PaymentId), false)
		}
	})
}

func TestFeeToUsd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, feeInUSDC, 0.50)
	})
}

type mockCircleApiClient struct {
	GetTransactionFunc func(
		ctx context.Context,
		id string,
	) (*GetTransactionResult, error)
	CreateTransferTransactionFunc func(
		ctx context.Context,
		idempotencyKey server.Id,
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
	idempotencyKey server.Id,
	amount float64,
	destinationAddress string,
	network string,
) (*CreateTransferTransactionResult, error) {
	return m.CreateTransferTransactionFunc(
		ctx,
		idempotencyKey,
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
	idempotencyKey server.Id,
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
