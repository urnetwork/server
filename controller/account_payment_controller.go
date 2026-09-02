package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	// "errors"

	// "sync"
	"time"

	mathrand "math/rand"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type GetNetworkAccountPaymentsError struct {
	Message string `json:"message"`
}

type GetNetworkAccountPaymentsResult struct {
	AccountPayments []*model.AccountPayment         `json:"account_payments,omitempty"`
	Error           *GetNetworkAccountPaymentsError `json:"error,omitempty"`
}

func GetNetworkAccountPayments(session *session.ClientSession) (*GetNetworkAccountPaymentsResult, error) {
	networkAccountPayments, err := model.GetNetworkPayments(session)

	if err != nil {
		return &GetNetworkAccountPaymentsResult{
			Error: &GetNetworkAccountPaymentsError{
				Message: err.Error(),
			},
		}, err
	}

	return &GetNetworkAccountPaymentsResult{
		AccountPayments: networkAccountPayments,
	}, nil
}

func TransferStats(session *session.ClientSession) (*model.TransferStats, error) {
	return model.GetTransferStats(session.Ctx, session.ByJwt.NetworkId), nil
}

func SchedulePendingPayments(clientSession *session.ClientSession) {
	pendingPayments := model.GetPendingPayments(clientSession.Ctx)
	for _, payment := range pendingPayments {
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			ScheduleAdvancePayment(&AdvancePaymentArgs{
				PaymentId: payment.PaymentId,
			}, clientSession, tx)
		})
	}
}

// PaymentPlanSliceDuration is the preferred bound for each committed payout
// transaction. It must remain longer than the configured minimum subsidy
// duration: a shorter slice can commit pass-through revenue without recording a
// subsidy epoch, leaving the bounded payout frontier unable to advance.
const PaymentPlanSliceDuration = 4 * 24 * time.Hour

const paymentPlanSliceSafetyMargin = 24 * time.Hour

func boundedPaymentPlanSliceDuration(minSubsidyDuration time.Duration) time.Duration {
	return max(
		PaymentPlanSliceDuration,
		minSubsidyDuration+paymentPlanSliceSafetyMargin,
	)
}

type paymentPlanLoop func(
	context.Context,
	time.Duration,
	func(*model.PaymentPlan),
) ([]*model.PaymentPlan, error)

type missingWalletNotice struct {
	paymentId server.Id
	payout    model.NanoCents
}

func collectMissingWalletNotices(plans []*model.PaymentPlan) map[server.Id]*missingWalletNotice {
	notices := map[server.Id]*missingWalletNotice{}
	for _, plan := range plans {
		for networkId, payment := range plan.NetworkPayments {
			if payment.WalletId != nil {
				continue
			}
			notice, ok := notices[networkId]
			if !ok {
				notice = &missingWalletNotice{paymentId: payment.PaymentId}
				notices[networkId] = notice
			}
			notice.payout += payment.Payout
		}
	}
	return notices
}

func SendPayments(clientSession *session.ClientSession) error {
	return sendPaymentsWithPlanner(clientSession, model.PlanPaymentsWithMaxDurationLoop)
}

func sendPaymentsWithPlanner(clientSession *session.ClientSession, planner paymentPlanLoop) error {
	plans, planErr := planner(
		clientSession.Ctx,
		boundedPaymentPlanSliceDuration(model.EnvSubsidyConfig().MinDurationPerPayout()),
		nil,
	)

	// Several slices can include the same network. Notify it at most once per
	// payout run instead of once per committed slice.
	missingWalletNotices := collectMissingWalletNotices(plans)

	// For any network that is missing a wallet id, send one notice carrying the
	// total withheld across every slice in this run.
	for networkId, notice := range missingWalletNotices {
		userAuth, err := model.GetUserAuth(clientSession.Ctx, networkId)
		if err == nil {
			awsMessageSender := GetAWSMessageSender()
			// TODO handler error

			awsMessageSender.SendAccountMessageTemplate(userAuth, &MissingWalletTemplate{
				PaymentId: notice.paymentId,
				AmountUsd: fmt.Sprintf("%.2f", model.NanoCentsToUsd(notice.payout)),
			})
		} else {
			glog.Warningf("[%s]Missing user auth. Cannot send missing wallet notice.", networkId)
		}
	}

	// schedule all pending payments, which includes the payments in this plan
	// and payments held from earlier plans (e.g. waiting on a valid wallet)
	SchedulePendingPayments(clientSession)

	// The loop can return already-committed plans together with an error from a
	// later slice. Those durable payments were scheduled above; return the error
	// so the payout task still retries the remaining frontier.
	return planErr
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
	// randomly schedule between now and 5 minutes from now
	minDelay := 5 * time.Minute
	delay := 25 * time.Minute
	// this avoid circle and coinbase rate limiting
	timeout := minDelay + time.Duration(mathrand.Float64()*float64(delay/time.Second))*time.Second
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
	if payment == nil || err != nil {
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
		// the payment will get picked up in the next dangling payment sweep. No need to keep trying until then.
		return &AdvancePaymentResult{
			Complete: false,
			Canceled: true,
		}, nil
	}

	complete, canceled, err := advancePayment(payment, clientSession)
	return &AdvancePaymentResult{
		Complete: complete,
		Canceled: canceled,
	}, err
}

func AdvancePaymentPost(
	advancePaymentArgs *AdvancePaymentArgs,
	advancePaymentResult *AdvancePaymentResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if !advancePaymentResult.Canceled && !advancePaymentResult.Complete {
		// keep checking on the payment until it is completed or canceled
		ScheduleAdvancePayment(advancePaymentArgs, clientSession, tx)
	}
	return nil
}

func advancePayment(
	payment *model.AccountPayment,
	clientSession *session.ClientSession,
) (complete bool, canceled bool, returnErr error) {
	if payment.Completed || payment.Canceled {
		complete = payment.Completed
		canceled = payment.Canceled
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
		txResult, err := circleClient.GetTransaction(clientSession.Ctx, *payment.PaymentRecord)
		if err != nil {
			returnErr = fmt.Errorf("[%s]Payment transaction error = %s", payment.PaymentId, err)
			return
		}

		tx = &txResult.Transaction
		txResponseBodyBytes = txResult.ResponseBodyBytes
		status = tx.State

		// Check the Circle status of the payment. Every non-terminal state stays
		// in retry; age is not a cancellation condition.
		switch strings.ToUpper(status) {
		case "INITIATED", "PENDING_RISK_SCREENING", "CLEARED", "QUEUED":
			// check later
			return

		case "SENT", "STUCK", "CONFIRMED":
			// Circle has assigned a chain transaction hash by SENT. Persist it
			// before terminal completion so a long-running retry remains
			// externally reconcilable and visible to the account holder.
			if tx.TxHash != "" {
				if err := model.UpdatePaymentProgress(
					clientSession.Ctx,
					payment.PaymentId,
					string(txResponseBodyBytes),
					tx.TxHash,
				); err != nil {
					returnErr = fmt.Errorf("[%s]Payment progress error = %s", payment.PaymentId, err)
				}
			}
			return

		case "DENIED", "FAILED":
			returnErr = fmt.Errorf("[%s]error = %s", payment.PaymentId, status)
			// remove the payment record so it can be recreated
			if err := model.RemovePaymentRecord(
				clientSession.Ctx,
				payment.PaymentId,
			); err != nil {
				returnErr = fmt.Errorf("[%s]error = %s; payment reset error = %s", payment.PaymentId, status, err)
			}
			return

		case "CANCELLED":
			// A chain hash and CANCELLED are contradictory. Preserve both pieces
			// of evidence and keep reconciling; releasing the sweeps here could
			// pay an already-broadcast transaction twice.
			if tx.TxHash != "" || payment.TxHash != nil {
				txHash := tx.TxHash
				if txHash == "" {
					txHash = *payment.TxHash
				}
				if err := model.UpdatePaymentProgress(
					clientSession.Ctx,
					payment.PaymentId,
					string(txResponseBodyBytes),
					txHash,
				); err != nil {
					returnErr = fmt.Errorf("[%s]Payment progress error = %s", payment.PaymentId, err)
					return
				}
				returnErr = fmt.Errorf("[%s]Circle returned CANCELLED for a payment with transaction hash %s", payment.PaymentId, txHash)
				return
			}
			if err := model.CancelPaymentAfterProcessorCancellation(
				clientSession.Ctx,
				payment.PaymentId,
				string(txResponseBodyBytes),
			); err != nil {
				returnErr = fmt.Errorf("[%s]Payment cancellation error = %s", payment.PaymentId, err)
				return
			}
			canceled = true
			return

		case "COMPLETE":

			// mark the payment complete in our DB
			if err := model.CompletePayment(
				clientSession.Ctx,
				payment.PaymentId,
				string(txResponseBodyBytes),
				tx.TxHash,
			); err != nil {
				returnErr = fmt.Errorf("[%s]Payment completion error = %s", payment.PaymentId, err)
				return
			}
			complete = true
			// no per-payment email: earnings are reported once per finalized
			// epoch by the epoch earnings email (controller/epoch_earnings_email.go)

			return

		default:
			returnErr = fmt.Errorf(
				"[%s]unknown status = %s",
				payment.PaymentId,
				status,
			)
			return
		}

	} else {
		// no transaction or error
		// create and send a new payment via Circle

		// get the user wallet to send the payment to
		accountWallet := model.GetAccountWallet(clientSession.Ctx, *payment.WalletId)
		// never send funds to a missing, deactivated, or foreign wallet.
		// like the missing wallet case, hold the payment until the network
		// sets a valid payout wallet. The pending payment sweep will retry it.
		if accountWallet == nil || !accountWallet.Active || accountWallet.NetworkId != payment.NetworkId {
			glog.Warningf("[%s]payment wallet %s is not a valid payout wallet for network %s. Holding payment.\n", payment.PaymentId, *payment.WalletId, payment.NetworkId)
			canceled = true
			return
		}
		formattedBlockchain, err := formatBlockchain(accountWallet.Blockchain)
		if err != nil {
			returnErr = fmt.Errorf("[%s]Payment wallet error = %s", payment.PaymentId, err)
			return
		}

		payoutAmount := model.NanoCentsToUsd(payment.Payout)

		// feeInUSDC, err := func() (float64, error) {
		// 	estimatedFees, err := circleClient.EstimateTransferFee(
		// 		clientSession.Ctx,
		// 		payoutAmount,
		// 		accountWallet.WalletAddress,
		// 		formattedBlockchain,
		// 	)
		// 	if err != nil {
		// 		return 0, fmt.Errorf("[%s]Payment fee estimate error = %s", payment.PaymentId, err)
		// 	}

		// 	fee, err := CalculateFee(*estimatedFees.Medium, formattedBlockchain)
		// 	if err != nil {
		// 		return 0, err
		// 	}

		// 	feeInUSDC, err := ConvertFeeToUSDC(clientSession.Ctx, formattedBlockchain, *fee)
		// 	if err != nil {
		// 		return 0, fmt.Errorf("[%s]Payment fee conversion error = %s", payment.PaymentId, err)
		// 	}

		// 	return feeInUSDC, nil
		// }()
		// if err != nil {
		// just choose a reasonable value
		// glog.Infof("[payout][%s]fee estimate failed. Using default fee. err = %s\n", payment.PaymentId, err)
		feeInUSDC := 0.01
		// }

		payoutAmount = payoutAmount - feeInUSDC

		// ensure paymout amount is greater than minimum payout threshold
		if model.UsdToNanoCents(payoutAmount) <= 0 {
			// cancel this payment, and let the next plan pick up the contracts
			// in a new (larger) payment. Otherwise, we will likely keep failing due
			// to the payment not being large enough to cover the transfer fee.
			glog.Info("[payout][%s]payout - fee is negative\n", payment.PaymentId)

			if err := model.CancelPayment(clientSession.Ctx, payment.PaymentId); err != nil {
				returnErr = fmt.Errorf("[%s]Payment cancellation error = %s", payment.PaymentId, err)
				return
			}
			canceled = true
			return
		}

		// the idempotency key is stable across retries of this payment.
		// creating it also pins the payment wallet (`UpdatePaymentWallet`),
		// so a retried submit pays the same address the processor already saw
		idempotencyKey, err := model.GetOrCreatePaymentIdempotencyKey(clientSession.Ctx, payment.PaymentId)
		if err != nil {
			returnErr = fmt.Errorf("[%s]Payment idempotency key error = %s", payment.PaymentId, err)
			return
		}

		// send the payment
		transferResult, err := circleClient.CreateTransferTransaction(
			clientSession.Ctx,
			idempotencyKey,
			payoutAmount,
			accountWallet.WalletAddress,
			formattedBlockchain,
		)
		if err != nil {
			auditAccountPayment(clientSession, payment.PaymentId, err)
			if isCircleInvalidDestinationError(err) {
				// Circle rejected the destination before creating a transfer, so
				// this is the one submit error for which it is safe to release the
				// pinned attempt. On the next retry UpdatePaymentWallet can select a
				// corrected payout wallet. Ambiguous failures retain the key.
				if resetErr := model.RemovePaymentRecord(clientSession.Ctx, payment.PaymentId); resetErr != nil {
					returnErr = fmt.Errorf("[%s]Payment create transaction error = %s; invalid destination reset error = %s", payment.PaymentId, err, resetErr)
					return
				}
			}
			returnErr = fmt.Errorf("[%s]Payment create transaction error = %s", payment.PaymentId, err)
			return
		}

		// set the payment record
		err = model.SetPaymentRecord(
			clientSession.Ctx,
			payment.PaymentId,
			"USDC", // For token type
			payoutAmount,
			transferResult.Id,
		)
		if err != nil {
			// the transfer was already submitted. Return an error so the task
			// retries; the stable idempotency key makes the resubmit safe.
			returnErr = fmt.Errorf("[%s]Payment record error = %s", payment.PaymentId, err)
			return
		}
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

func ConvertFeeToUSDC(ctx context.Context, currencyTicker string, fee float64) (float64, error) {

	currencyTicker = strings.ToUpper(currencyTicker)

	coinbaseClient := NewCoinbaseClient()

	ratesResult, err := coinbaseClient.FetchExchangeRates(ctx, currencyTicker)
	if err != nil {
		return 0, err
	}

	rateStr, exists := ratesResult.Rates["USDC"]
	if !exists {
		return 0, fmt.Errorf("currency ticker not found for %s", currencyTicker)
	}

	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse rate: %v", err)
	}

	feeUsdc := fee * rate

	return feeUsdc, nil
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
