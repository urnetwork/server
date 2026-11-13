package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

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
	Asset           string                          `json:"asset"`
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
		Asset:           "USDC",
		AccountPayments: networkAccountPayments,
	}, nil
}

func TransferStats(session *session.ClientSession) (*model.TransferStats, error) {
	return model.GetTransferStats(session.Ctx, session.ByJwt.NetworkId), nil
}

// While USDC payments are off, only a payment Circle already holds is
// scheduled, for its status check; every other pending payment stays as it is
// and no longer cycles.
func SchedulePendingPayments(clientSession *session.ClientSession) {
	usdcPayout, _ := loadUsdcPayoutPolicy()
	pendingPayments := model.GetPendingPayments(clientSession.Ctx)
	for _, payment := range pendingPayments {
		if !usdcPayout.PaymentsOn() && payment.PaymentRecord == nil {
			continue
		}
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			ScheduleAdvancePayment(&AdvancePaymentArgs{
				PaymentId: payment.PaymentId,
			}, clientSession, tx)
		})
	}
}

// The held reason of a USDC payout while payments are off.
const usdcPaymentsOffReason = "usdc payments are off (usdc_payout.yml)"

// The refusal of a USDC send while payments are off.
var ErrUsdcPaymentsOff = errors.New("usdc payments are off (usdc_payout.yml); turn payments on to send")

// Reports an unreadable usdc_payout.yml at most once a minute.
var usdcPayoutPolicyErrorReports = newBoundedReport(time.Minute)

// The usdc_payout.yml switches. A file that cannot be read or parsed asks no
// network to connect a wallet and sends nothing; the error is reported and
// returned for callers that surface it.
func loadUsdcPayoutPolicy() (*server.UsdcPayoutPolicy, error) {
	policy, err := server.LoadUsdcPayoutPolicy()
	if err != nil {
		if suppressedCount, ok := usdcPayoutPolicyErrorReports.Allow(server.NowUtc()); ok {
			glog.Infof("[payout]%s: asking no network to connect a wallet and sending no usdc; %d more since the last report\n", err, suppressedCount)
		}
		return &server.UsdcPayoutPolicy{
			WalletRequests: server.UsdcPayoutOff,
			Payments:       server.UsdcPayoutOff,
		}, err
	}
	return policy, nil
}

// Whether a withheld payout may ask its network to connect a wallet.
func usdcWalletRequestsOn() bool {
	policy, _ := loadUsdcPayoutPolicy()
	return policy.WalletRequestsOn()
}

// Refuses a USDC send while payments are off.
func RequireUsdcPaymentsOn() error {
	policy, err := loadUsdcPayoutPolicy()
	if err != nil {
		return errors.Join(ErrUsdcPaymentsOff, err)
	}
	if !policy.PaymentsOn() {
		return ErrUsdcPaymentsOff
	}
	return nil
}

// Advance payment handles a single payment until completion

// The fixed transfer fee a fresh attempt deducts from the payout. The
// sn-reconcile census deducts the same fee to report each send amount.
const providerPaymentFeeUsd = 0.01

type AdvancePaymentArgs struct {
	PaymentId server.Id `json:"payment_id"`
}

type AdvancePaymentResult struct {
	Complete   bool   `json:"complete"`
	Canceled   bool   `json:"canceled"`
	Retryable  bool   `json:"retryable,omitempty"`
	HeldReason string `json:"held_reason,omitempty"`
	// USDC payments are off and Circle does not hold the payment: nothing was
	// submitted, and the payment is not scheduled again
	PaymentsOff bool `json:"payments_off,omitempty"`
}

func ScheduleAdvancePayment(
	advancePaymentArgs *AdvancePaymentArgs,
	clientSession *session.ClientSession,
	tx server.PgTx,
) {
	// Randomly schedule between 5 and 30 minutes from now.
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
	usdcPayout, usdcPayoutErr := loadUsdcPayoutPolicy()
	if usdcPayout.PaymentsOn() {
		model.UpdatePaymentWallet(clientSession.Ctx, advancePaymentArgs.PaymentId)
	}
	payment, err := model.GetPayment(clientSession.Ctx, advancePaymentArgs.PaymentId)
	if err != nil {
		return &AdvancePaymentResult{}, err
	}
	if payment == nil {
		// payment doesn't exist
		return &AdvancePaymentResult{
			Complete: false,
			Canceled: true,
		}, nil
	}
	if observer, ok := clientSession.Ctx.Value(providerPaymentReadObserverKey{}).(func(*model.AccountPayment)); ok {
		observer(payment)
	}

	if payment.Completed || payment.Canceled {
		return &AdvancePaymentResult{
			Complete: payment.Completed,
			Canceled: payment.Canceled,
		}, nil
	}

	if !usdcPayout.PaymentsOn() && payment.PaymentRecord == nil {
		// Circle does not hold this payment, so nothing is reserved or
		// submitted: an attempt keyed before payments turned off stays retained
		// for reconciliation. A payment Circle holds keeps its status check
		// below, so no sent transfer goes untracked.
		return &AdvancePaymentResult{
			PaymentsOff: true,
			HeldReason:  usdcPaymentsOffReason,
		}, usdcPayoutErr
	}

	if payment.WalletId == nil && payment.PaymentRecord == nil {
		// cannot advance until the wallet is set
		// the payment will get picked up in the next dangling payment sweep. No need to keep trying until then.
		return &AdvancePaymentResult{
			Complete: false,
			Canceled: false,
		}, fmt.Errorf("payment retained pending a payout wallet")
	}

	if payment.PaymentRecord == nil && payment.CircleIdempotencyKey == nil {
		// a payment becomes payable only after its plan's staged sweeps are all
		// assigned to it; the payout run schedules it again when they are
		pending, err := model.PaymentPlanAssignmentPending(clientSession.Ctx, payment.PaymentPlanId)
		if err != nil {
			return &AdvancePaymentResult{}, err
		}
		if pending {
			return &AdvancePaymentResult{
				Complete: false,
				Canceled: false,
			}, fmt.Errorf("payment retained pending its plan's sweep assignment")
		}
	}

	complete, canceled, err := advancePayment(payment, clientSession)
	if server.ProviderEarningBoundaryRetryable(clientSession.Ctx, err) {
		// The post consumer schedules one bounded RunOnce continuation. This is
		// an unavailable observation, never completion/cancellation or new money.
		return &AdvancePaymentResult{Retryable: true, HeldReason: err.Error()}, nil
	}
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
	if err := clientSession.Ctx.Err(); err != nil {
		return err
	}
	if !advancePaymentResult.Canceled && !advancePaymentResult.Complete && !advancePaymentResult.PaymentsOff {
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
			returnErr = fmt.Errorf("[%s]Payment transaction error: %w", payment.PaymentId, err)
			return
		}

		tx = &txResult.Transaction
		txResponseBodyBytes = txResult.ResponseBodyBytes
		status = tx.State
		if tx.Id != "" && tx.Id != *payment.PaymentRecord {
			return false, false, fmt.Errorf("processor returned a different transaction; original attempt retained")
		}

		// Check the Circle status of the payment. Every non-terminal state stays
		// in retry; age is not a cancellation condition.
		switch strings.ToUpper(status) {
		case "INITIATED", "PENDING_RISK_SCREENING", "CLEARED", "QUEUED":
			// check later
			return

		case "SENT", "STUCK", "CONFIRMED":
			return model.ApplyProviderPaymentOutcome(clientSession.Ctx, payment, status, string(txResponseBodyBytes), tx.TxHash, false)

		case "DENIED", "FAILED":
			complete, canceled, err = model.ApplyProviderPaymentOutcome(clientSession.Ctx, payment, status, string(txResponseBodyBytes), tx.TxHash, false)
			return complete, canceled, errors.Join(fmt.Errorf("[%s]processor status = %s; obligation retained for retry", payment.PaymentId, status), err)

		case "CANCELLED":
			return model.ApplyProviderPaymentOutcome(clientSession.Ctx, payment, status, string(txResponseBodyBytes), tx.TxHash, false)

		case "COMPLETE":
			review := model.RequireProviderUsdcPayment(clientSession.Ctx, payment.PaymentId) != nil
			return model.ApplyProviderPaymentOutcome(clientSession.Ctx, payment, status, string(txResponseBodyBytes), tx.TxHash, review)

		default:
			returnErr = fmt.Errorf(
				"[%s]unknown status = %s",
				payment.PaymentId,
				status,
			)
			return
		}

	} else {
		var request *model.ProviderPaymentRequest
		if payment.CircleIdempotencyKey != nil {
			var err error
			request, err = model.GetRetainedProviderPaymentRequest(clientSession.Ctx, payment)
			if err != nil {
				return false, false, err
			}
		} else {
			// Only a fresh attempt consults current wallet selection and fees.

			// get the user wallet to send the payment to
			accountWallet := model.GetAccountWallet(clientSession.Ctx, *payment.WalletId)
			// never send funds to a missing, deactivated, or foreign wallet.
			// like the missing wallet case, hold the payment until the network
			// sets a valid payout wallet. The pending payment sweep will retry it.
			if accountWallet == nil || !accountWallet.Active || accountWallet.NetworkId != payment.NetworkId {
				glog.Warningf("[%s]payment wallet %s is not a valid payout wallet for network %s. Holding payment.\n", payment.PaymentId, *payment.WalletId, payment.NetworkId)
				return false, false, fmt.Errorf("payment retained pending a valid payout wallet")
			}
			formattedBlockchain, err := formatBlockchain(accountWallet.Blockchain)
			if err != nil {
				returnErr = fmt.Errorf("[%s]Payment wallet error: %w", payment.PaymentId, err)
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
			feeInUSDC := providerPaymentFeeUsd
			// }

			payoutAmount = payoutAmount - feeInUSDC

			// ensure paymout amount is greater than minimum payout threshold
			if model.UsdToNanoCents(payoutAmount) <= 0 {
				policy, err := server.LoadProviderPayoutEarningPolicy(clientSession.Ctx)
				if err != nil {
					return false, false, err
				}
				if policy != nil {
					return false, false, fmt.Errorf("legacy payment retained: amount does not cover the transfer fee")
				}
				// cancel this payment, and let the next plan pick up the contracts
				// in a new (larger) payment. Otherwise, we will likely keep failing due
				// to the payment not being large enough to cover the transfer fee.
				glog.Infof("[payout][%s]payout - fee is negative\n", payment.PaymentId)

				if err := model.CancelPayment(clientSession.Ctx, payment.PaymentId); err != nil {
					returnErr = fmt.Errorf("[%s]Payment cancellation error: %w", payment.PaymentId, err)
					return
				}
				canceled = true
				return
			}

			// The key and exact request become durable together before sending.
			request, err = model.ReserveProviderPaymentRequest(clientSession.Ctx, payment, accountWallet, payoutAmount, formattedBlockchain)
			if err != nil {
				returnErr = fmt.Errorf("[%s]Payment idempotency key error = %w", payment.PaymentId, err)
				return
			}
		}
		basis, payoutAmount, formattedBlockchain := &request.Basis, request.Amount, request.Network

		// send the payment
		transferResult, err := circleClient.CreateTransferTransaction(
			context.WithValue(clientSession.Ctx, providerUsdcPaymentContextKey{}, providerPaymentSubmission{Basis: *basis, Amount: payoutAmount, Network: formattedBlockchain}),
			basis.IdempotencyKey,
			payoutAmount,
			basis.WalletAddress,
			formattedBlockchain,
		)
		if err != nil {
			auditAccountPayment(clientSession, payment.PaymentId, err)
			if isCircleInvalidDestinationError(err) {
				// Circle rejected the destination before creating a transfer, so
				// this is the one submit error for which it is safe to release the
				// pinned attempt. On the next retry UpdatePaymentWallet can select a
				// corrected payout wallet. Ambiguous failures retain the key.
				if resetErr := model.ResetProviderPaymentSubmission(clientSession.Ctx, basis, err.Error()); resetErr != nil {
					returnErr = fmt.Errorf("[%s]Payment create transaction error: %w; invalid destination reset error: %w", payment.PaymentId, err, resetErr)
					return
				}
			}
			returnErr = fmt.Errorf("[%s]Payment create transaction error = %w", payment.PaymentId, err)
			return
		}

		// set the payment record
		err = model.SetProviderPaymentRecord(
			clientSession.Ctx,
			basis,
			payoutAmount,
			transferResult.Id,
		)
		if err != nil {
			// the transfer was already submitted. Return an error so the task
			// retries; the stable idempotency key makes the resubmit safe.
			returnErr = fmt.Errorf("[%s]Payment record error: %w", payment.PaymentId, err)
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
		return 0, fmt.Errorf("failed to parse rate: %w", err)
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
