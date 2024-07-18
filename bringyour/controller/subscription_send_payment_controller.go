package controller

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/task"
)

var (
	processedPayments = make(map[bringyour.Id]struct{})
	mu sync.Mutex
)

func isBeingProcessed(paymentId bringyour.Id) bool {
	mu.Lock()
	defer mu.Unlock()

	_, exists := processedPayments[paymentId]
	return exists
}

func markAsProcessed(paymentId bringyour.Id) {
	mu.Lock()
	defer mu.Unlock()

	processedPayments[paymentId] = struct{}{}
}


// run once on startup
func SchedulePendingPayments(session *session.ClientSession) {

	pendingPayments := model.GetPendingPayments(session.Ctx)

	// schedule a task for CoinbasePayment for each paymentId
	// (use balk because the payment id might already be worked on)
	for _, payment := range pendingPayments {
		if isBeingProcessed(payment.PaymentId) || payment.Completed || payment.Canceled  {
			continue
		}

		// avoid circl rate limiting
		time.Sleep(500 * time.Millisecond)

		markAsProcessed(payment.PaymentId)

		task.ScheduleTask(
			ProviderPayout,
			payment,
			session,
		)
	}
}

// runs twice a day
func SendPayments(session *session.ClientSession) {

	plan := model.PlanPayments(session.Ctx)

	// create coinbase payment records
	// schedule a task for CoinbasePayment for each paymentId
	// (use balk because the payment id might already be worked on)
	for _, payment := range plan.WalletPayments {
		if isBeingProcessed(payment.PaymentId) || payment.Completed || payment.Canceled {
			continue
		}

		// avoid circl rate limiting
		time.Sleep(500 * time.Millisecond)

		markAsProcessed(payment.PaymentId)

		task.ScheduleTask(
			ProviderPayout,
			payment,
			session,
		)

	}
	
}


// TODO start a task to retry a payment until it completes
// payment_id
func RetryPayment(payment_id bringyour.Id, session session.ClientSession) {
	// get the payment
	// schedule a task for CoinbasePayment for the payment
	// (use balk because the payment id might already be worked on)
	payment, err := model.GetPayment(session.Ctx, payment_id)
	if err != nil {
		bringyour.Logger().Println("RetryPayment - Error getting payment", err)
		return
	}

	if isBeingProcessed(payment.PaymentId) || payment.Completed || payment.Canceled {
		return
	}

	markAsProcessed(payment.PaymentId)

	task.ScheduleTask(
		ProviderPayout,
		payment,
		&session,
	)
}

type ProviderPayoutResult struct {
	Complete bool
}

func ProviderPayout(
	payment *model.AccountPayment,
	clientSession *session.ClientSession,
) (*ProviderPayoutResult, error) {

	// if has payment record, get the status of the transaction
	// if complete, finish and send email
	// if in progress, wait
	// payment := circlePayment.Payment
	circleClient := NewCircleClient()

	// GET https://api.coinbase.com/v2/accounts/:account_id/transactions/:transaction_id
	// https://docs.cloud.coinbase.com/sign-in-with-coinbase/docs/api-transactions

	if payment.Completed || payment.Canceled {
		return &ProviderPayoutResult{
			Complete: true,
		}, nil
	}

	var tx *CircleTransaction
	var txResponseBodyBytes []byte // STU_TODO: this is ResponseBodyBytes from fetching the transaction data?
	var status string

	if payment.PaymentRecord != "" {

		// get the status of the transaction
		txResult, err := circleClient.GetTransaction(payment.PaymentRecord)
		if err != nil {
			return nil, err
		}

		tx = &txResult.Transaction
		txResponseBodyBytes = txResult.ResponseBodyBytes
		status = tx.State
	}

	// INITIATED, PENDING_RISK_SCREENING, DENIED, QUEUED, SENT, CONFIRMED, COMPLETE, FAILED, CANCELLED
	switch status {
	case "INITIATED", "PENDING_RISK_SCREENING", "QUEUED", "SENT", "CONFIRMED":
		// check later	
		return &ProviderPayoutResult{
			Complete: false,
		}, nil

	case "DENIED", "FAILED", "CANCELLED":

		// Cancel this payment in our DB
		err := model.CancelPayment(clientSession.Ctx, payment.PaymentId)
		if err != nil {
			return nil, err
		}

		// Returns complete, since we don't want to retry this payment
		return &ProviderPayoutResult{
			Complete: true,
		}, nil

	case "COMPLETE":

		// mark the payment complete in our DB
		model.CompletePayment(
			clientSession.Ctx, 
			payment.PaymentId, 
			string(txResponseBodyBytes), // STU_TODO: check this
		)

		userAuth, err := model.GetUserAuth(clientSession.Ctx, payment.NetworkId)
		if err != nil {
			return nil, err
		}

		awsMessageSender := GetAWSMessageSender()
		// TODO handler error
		awsMessageSender.SendAccountMessageTemplate(userAuth, &SendPaymentTemplate{})

		return &ProviderPayoutResult{
			Complete: true,
		}, nil

	default:
		// no transaction or error
		// send the payment

		// get the user wallet to send the payment to
		accountWallet := model.GetAccountWallet(clientSession.Ctx, payment.WalletId)

		// get the admin wallet to send the payment from
		adminWalletId, err := getWalletIdByNetwork(accountWallet.Blockchain)
		if err != nil {
				return nil, err
		}

		payoutAmount := model.NanoCentsToUsd(payment.Payout)

		estimatedFees, err := circleClient.EstimateTransferFee(
			payoutAmount, // TODO check this
			accountWallet.WalletAddress,
			accountWallet.Blockchain,
		)
		if err != nil {
			return nil, err
		}

		fee, err := CalculateFee(*estimatedFees.Medium, accountWallet.Blockchain)
		if err != nil {
			return nil, err
		}

		feeInUSDC, err := ConvertFeeToUSDC("MATIC", *fee)
		if err != nil {
			return nil, err
		}

		usdcNetworkAddress, err := getUsdcAddressByNetwork(accountWallet.Blockchain)
		if err != nil {
				return nil, err
		}

		payoutAmount = payoutAmount - *feeInUSDC

		// send the payment
		transferResult, err := circleClient.CreateTransferTransaction(
			payoutAmount,
			accountWallet.WalletAddress,
			accountWallet.Blockchain,
			adminWalletId,
			usdcNetworkAddress,
		)
		if err != nil {
			return nil, err
		}

		// set the payment record
		model.SetPaymentRecord(
			clientSession.Ctx,
			payment.PaymentId,
			payment.TokenType,
			payoutAmount,
			transferResult.Id,
		)

		return &ProviderPayoutResult{
			Complete: false,
		}, nil
	}
}


func CalculateFee(feeEstimate FeeEstimate, network string) (*float64, error) {
	
	switch network {
	case "SOL", "SOLANA":
		return calculateFeeSolana(feeEstimate)
	case "MATIC":
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