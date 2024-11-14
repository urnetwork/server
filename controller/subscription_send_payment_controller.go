package controller

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	// "sync"
	"time"

	mathrand "math/rand"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func SchedulePendingPayments(clientSession *session.ClientSession) {
	pendingPayments := model.GetPendingPayments(clientSession.Ctx)
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		for _, payment := range pendingPayments {
			ScheduleAdvancePayment(&AdvancePaymentArgs{
				PaymentId: payment.PaymentId,
			}, clientSession, tx)
		}
	})
}

func SendPayments(clientSession *session.ClientSession) error {
	plan, err := model.PlanPayments(clientSession.Ctx)
	if err != nil {
		return err
	}

	// for any newtork that is missing a wallet id, send a notice
	for networkId, payment := range plan.NetworkPayments {
		if payment.WalletId == nil {
			userAuth, err := model.GetUserAuth(clientSession.Ctx, networkId)
			if err != nil {
				return err
			}

			awsMessageSender := GetAWSMessageSender()
			// TODO handler error

			awsMessageSender.SendAccountMessageTemplate(userAuth, &MissingWalletTemplate{
				PaymentId: payment.PaymentId,
				AmountUsd: fmt.Sprintf("%.2f", model.NanoCentsToUsd(payment.Payout)),
			})
		}
	}

	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		for _, payment := range plan.NetworkPayments {
			ScheduleAdvancePayment(&AdvancePaymentArgs{
				PaymentId: payment.PaymentId,
			}, clientSession, tx)
		}
	})

	return nil
}

// run at start
type ProcessPendingPayoutsArgs struct {
}

type ProcessPendingPayoutsResult struct {
}

func ScheduleProcessPendingPayouts(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		ProcessPendingPayouts,
		&ProcessPendingPayoutsArgs{},
		clientSession,
		task.RunOnce("process_pending_payouts"),
	)
}

func ProcessPendingPayouts(
	processPending *ProcessPendingPayoutsArgs,
	clientSession *session.ClientSession,
) (*ProcessPendingPayoutsResult, error) {
	// send a continuous verification code message to a bunch of popular email providers

	SchedulePendingPayments(clientSession)

	return &ProcessPendingPayoutsResult{}, nil
}

func ProcessPendingPayoutsPost(
	processPendingArgs *ProcessPendingPayoutsArgs,
	processPendingResult *ProcessPendingPayoutsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}

// Advance payment handles a single payment until completion

type AdvancePaymentArgs struct {
	PaymentId server.Id `json:"payment_id"`
}

type AdvancePaymentResult struct {
	Complete bool `json:"complete"`
	Canceled bool `json:"canceled"`
}

func ScheduleAdvancePayment(
	advancePaymentArgs *AdvancePaymentArgs,
	clientSession *session.ClientSession,
	tx server.PgTx,
) {
	// randomly schedule between now and 1 hour from now
	// this avoid circle and coinbase rate limiting
	timeout := time.Duration(mathrand.Float64()*float64(1*time.Hour/time.Second)) * time.Second
	runAt := server.NowUtc().Add(timeout)

	task.ScheduleTaskInTx(
		tx,
		AdvancePayment,
		advancePaymentArgs,
		clientSession,
		task.RunOnce("advance_payment", advancePaymentArgs.PaymentId),
		task.RunAt(runAt),
	)
}

func AdvancePayment(
	advancePaymentArgs *AdvancePaymentArgs,
	clientSession *session.ClientSession,
) (*AdvancePaymentResult, error) {
	model.UpdatePaymentWallet(clientSession.Ctx, advancePaymentArgs.PaymentId)
	payment, err := model.GetPayment(clientSession.Ctx, advancePaymentArgs.PaymentId)
	if err != nil {
		// payment doesn't exist
		return &AdvancePaymentResult{
			Complete: false,
			Canceled: true,
		}, nil
	}

	if payment.Completed || payment.Canceled {
		return &AdvancePaymentResult{
			Complete: payment.Completed,
			Canceled: payment.Canceled,
		}, nil
	}

	if payment.WalletId == nil {
		// cannot advance until the wallet is set
		return &AdvancePaymentResult{}, nil
	}

	complete, err := advancePayment(payment, clientSession)
	if err != nil {
		// cancel this payment and let the payment plan create a new one
		cancelErr := model.CancelPayment(clientSession.Ctx, advancePaymentArgs.PaymentId)
		if cancelErr == nil {
			return &AdvancePaymentResult{
				Complete: false,
				Canceled: true,
			}, nil
		} else {
			// keep trying until we can cancel it
			return &AdvancePaymentResult{
				Complete: false,
				Canceled: false,
			}, nil
		}
	}
	return &AdvancePaymentResult{
		Complete: complete,
		Canceled: false,
	}, nil
}

func AdvancePaymentPost(
	advancePaymentArgs *AdvancePaymentArgs,
	advancePaymentResult *AdvancePaymentResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if !advancePaymentResult.Complete && !advancePaymentResult.Canceled {
		// keep checking on the payment until it is completed or canceled
		ScheduleAdvancePayment(advancePaymentArgs, clientSession, tx)
	}
	return nil
}

func advancePayment(
	payment *model.AccountPayment,
	clientSession *session.ClientSession,
) (complete bool, returnErr error) {
	if payment.Completed || payment.Canceled {
		complete = true
		return
	}

	// if has payment record, get the status of the transaction
	// if complete, finish and send email
	// if in progress, wait
	// payment := circlePayment.Payment
	circleClient := NewCircleClient()

	// GET https://api.coinbase.com/v2/accounts/:account_id/transactions/:transaction_id
	// https://docs.cloud.coinbase.com/sign-in-with-coinbase/docs/api-transactions

	// payment exists
	if payment.PaymentRecord != nil {
		var tx *CircleTransaction
		var txResponseBodyBytes []byte
		var status string

		// get the status of the transaction
		txResult, err := circleClient.GetTransaction(*payment.PaymentRecord)
		if err != nil {
			returnErr = err
			return
		}

		tx = &txResult.Transaction
		txResponseBodyBytes = txResult.ResponseBodyBytes
		status = tx.State

		// Check the Circle Status of the payment
		// INITIATED, PENDING_RISK_SCREENING, DENIED, QUEUED, SENT, CONFIRMED, COMPLETE, FAILED, CANCELLED
		switch strings.ToUpper(status) {
		case "INITIATED", "PENDING_RISK_SCREENING", "QUEUED", "SENT", "CONFIRMED":
			// check later
			return

		case "DENIED", "FAILED", "CANCELLED":

			returnErr = fmt.Errorf("[%s]error = %s", payment.PaymentId.String(), status)
			return

		case "COMPLETE":

			// mark the payment complete in our DB
			model.CompletePayment(
				clientSession.Ctx,
				payment.PaymentId,
				string(txResponseBodyBytes), // STU_TODO: check this
			)

			userAuth, err := model.GetUserAuth(clientSession.Ctx, payment.NetworkId)
			if err != nil {
				returnErr = err
				return
			}

			awsMessageSender := GetAWSMessageSender()
			// TODO handler error

			explorerBasePath := getExplorerTxPath(tx.Blockchain)

			networkReferralCode := model.GetNetworkReferralCode(clientSession.Ctx, payment.NetworkId)

			if networkReferralCode != nil {
				awsMessageSender.SendAccountMessageTemplate(userAuth, &SendPaymentTemplate{
					PaymentId:          payment.PaymentId,
					ExplorerBasePath:   *explorerBasePath,
					TxHash:             tx.TxHash,
					ReferralCode:       networkReferralCode.ReferralCode.String(),
					Blockchain:         tx.Blockchain,
					DestinationAddress: tx.DestinationAddress,
					AmountUsd:          tx.AmountInUSD,
					PaymentCreatedAt:   payment.CreateTime,
				})
			}

			complete = true
			return

		default:
			returnErr = fmt.Errorf(
				"[%s]unknown status = %s",
				payment.PaymentId.String(),
				status,
			)
			return
		}

	} else {
		// no transaction or error
		// create and send a new payment via Circle

		// get the user wallet to send the payment to
		accountWallet := model.GetAccountWallet(clientSession.Ctx, *payment.WalletId)
		formattedBlockchain, err := formatBlockchain(accountWallet.Blockchain)
		if err != nil {
			returnErr = err
			return
		}

		payoutAmount := model.NanoCentsToUsd(payment.Payout)

		estimatedFees, err := circleClient.EstimateTransferFee(
			payoutAmount,
			accountWallet.WalletAddress,
			formattedBlockchain,
		)
		if err != nil {
			returnErr = err
			return
		}

		fee, err := CalculateFee(*estimatedFees.Medium, formattedBlockchain)
		if err != nil {
			returnErr = err
			return
		}

		feeInUSDC, err := ConvertFeeToUSDC(formattedBlockchain, *fee)
		if err != nil {
			returnErr = err
			return
		}

		payoutAmount = payoutAmount - *feeInUSDC

		// ensure paymout amount is greater than minimum payout threshold
		if model.UsdToNanoCents(payoutAmount) <= 0 {
			returnErr = fmt.Errorf("[%s]payout - fee is negative", payment.PaymentId.String())
			return
		}

		// send the payment
		transferResult, err := circleClient.CreateTransferTransaction(
			payoutAmount,
			accountWallet.WalletAddress,
			formattedBlockchain,
		)
		if err != nil {
			auditAccountPayment(clientSession, payment.PaymentId, err)
			returnErr = err
			return
		}

		// set the payment record
		model.SetPaymentRecord(
			clientSession.Ctx,
			payment.PaymentId,
			"USDC", // For token type
			payoutAmount,
			transferResult.Id,
		)
	}
	return
}

func CalculateFee(feeEstimate FeeEstimate, network string) (*float64, error) {

	network = strings.ToUpper(network)

	switch network {
	case "SOL", "SOLANA":
		return calculateFeeSolana(feeEstimate)
	case "POLYGON", "MATIC":
		return calculateFeePolygon(feeEstimate)
	default:
		return nil, fmt.Errorf("unsupported network: %s", network)
	}

}

func calculateFeePolygon(feeEstimate FeeEstimate) (*float64, error) {

	gasLimit, err := strconv.ParseFloat(feeEstimate.GasLimit, 64)
	if err != nil {
		return nil, err
	}

	priorityFee, err := strconv.ParseFloat(feeEstimate.PriorityFee, 64)
	if err != nil {
		return nil, err
	}

	baseFee, err := strconv.ParseFloat(feeEstimate.BaseFee, 64)
	if err != nil {
		return nil, err
	}

	totalFeeGwei := gasLimit * (baseFee + priorityFee)

	totalFeeMATIC := totalFeeGwei * math.Pow(10, -9)

	return &totalFeeMATIC, nil
}

func calculateFeeSolana(feeEstimate FeeEstimate) (*float64, error) {

	gasLimit, err := strconv.ParseFloat(feeEstimate.GasLimit, 64)
	if err != nil {
		return nil, err
	}

	priorityFee, err := strconv.ParseFloat(feeEstimate.PriorityFee, 64)
	if err != nil {
		return nil, err
	}

	baseFee, err := strconv.ParseFloat(feeEstimate.BaseFee, 64)
	if err != nil {
		return nil, err
	}

	fee := baseFee + (gasLimit * priorityFee * math.Pow(10, -15))

	return &fee, nil
}

func ConvertFeeToUSDC(currencyTicker string, fee float64) (*float64, error) {

	currencyTicker = strings.ToUpper(currencyTicker)

	coinbaseClient := NewCoinbaseClient()

	ratesResult, err := coinbaseClient.FetchExchangeRates(currencyTicker)
	if err != nil {
		return nil, err
	}

	rateStr, exists := ratesResult.Rates["USDC"]
	if !exists {
		return nil, fmt.Errorf("currency ticker not found for %s", currencyTicker)
	}

	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse rate: %v", err)
	}

	feeUsdc := fee * rate

	return &feeUsdc, nil
}

func getExplorerTxPath(network string) *string {
	network = strings.ToUpper(network)

	switch network {
	case "SOL", "SOLANA":
		explorerPath := "https://explorer.solana.com/tx"
		return &explorerPath
	case "MATIC", "POLY", "POLYGON":
		explorerPath := "https://polygonscan.com/tx"
		return &explorerPath
	}

	return nil
}

func formatBlockchain(network string) (string, error) {
	network = strings.TrimSpace(network)
	network = strings.ToUpper(network)

	switch network {
	case "POLYGON", "POLY", "MATIC":
		return "MATIC", nil
	case "SOL", "SOLANA":
		return "SOL", nil
	default:
		return "", fmt.Errorf("unsupported chain: %s", network)
	}
}

func auditAccountPayment(
	session *session.ClientSession,
	paymentId server.Id,
	err error,
) {
	type Details struct {
		ErrorMsg string `json:"error"`
	}

	details := Details{
		ErrorMsg: err.Error(),
	}

	detailsJson, err := json.Marshal(details)
	if err != nil {
		panic(err)
	}
	detailsJsonString := string(detailsJson)

	auditNetworkEvent := model.NewAuditAccountPaymentEvent(model.AuditEventTypeCirclePayoutFailed)
	auditNetworkEvent.AccountPaymentId = paymentId
	auditNetworkEvent.EventDetails = &detailsJsonString
	model.AddAuditEvent(session.Ctx, auditNetworkEvent)
}
