package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// Recording a settled x402 payment in Stripe.
//
// x402 money does not flow through Stripe the way a checkout does. The agent
// signs a transfer, the facilitator settles it on chain, and the funds land in
// a Stripe crypto deposit address. Stripe only learns about it when we tell it:
// we create a PaymentIntent in `transaction_verification` mode naming the
// network and the transaction hash, and Stripe verifies that transfer on chain
// and books it.
//
// Without this step the transfer still arrives and the customer still gets what
// they paid for, but it never appears in Stripe -- no payment record, no
// reporting, no reconciliation against the rest of the payment stack. That is
// the gap this closes.
//
// IT MUST NEVER FAIL A PURCHASE. By the time this runs the money has moved and
// the entitlement is granted; neither can be taken back. A recording failure is
// a bookkeeping hole to be repaired from the logged transaction hash, not a
// reason to answer the agent with an error for something it successfully
// bought. Same rule as x402SendReceipt.

// stripePreviewApiVersion is the API version that carries crypto
// transaction_verification. Stripe gates the feature on the header, so a plain
// call without it is rejected.
const stripePreviewApiVersion = "2026-05-27.preview"

// usdcAtomicPerCent converts USDC atomic units (6 decimals) to cents:
// 1 cent = 10_000 atomic units.
const usdcAtomicPerCent = 10_000

// x402StripeNetworks maps our network names to the ones Stripe accepts for
// crypto transaction verification.
//
// An explicit map, not a pass-through: a name we have not confirmed with Stripe
// would be rejected at the far end with a message about a field, long after the
// money moved. Better to refuse here and say which network we could not record.
var x402StripeNetworks = map[string]string{
	"base":   "base",
	"solana": "solana",
	"tempo":  "tempo",
}

// x402StripeRecordEnabled is the seam hermetic tests use to keep this out of
// their way, and the switch that keeps the whole thing inert until Stripe's
// crypto payment method is actually enabled on the account. Production sets it
// by having a usable x402 config; see x402ConfigUsable.
var x402StripeRecordEnabledFunc = func() bool { return X402Enabled() }

// stripePaymentIntentResult is the slice of Stripe's PaymentIntent we care
// about: enough to log what was booked and to tell a refusal from a success.
type stripePaymentIntentResult struct {
	Id     string `json:"id"`
	Status string `json:"status"`
	Amount int64  `json:"amount"`
}

// x402RecordStripePayment books a settled x402 transfer as a Stripe
// PaymentIntent. Errors are logged, never returned.
func x402RecordStripePayment(
	ctx context.Context,
	sku *X402Sku,
	requirements *X402Accept,
	settleResponse *X402SettleResponse,
) {
	// A panic here would take down a request for a purchase that already
	// succeeded.
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[x402]stripe record panicked: %v\n", r)
		}
	}()

	if !x402StripeRecordEnabledFunc() {
		return
	}
	if sku == nil || requirements == nil || settleResponse == nil {
		glog.Errorf("[x402]stripe record called with nothing to record\n")
		return
	}

	transaction := settleResponse.Transaction
	if transaction == "" {
		// The facilitator settled but told us no transaction id, so there is
		// nothing for Stripe to verify. Worth saying out loud: it means the
		// settle response shape changed.
		glog.Errorf(
			"[x402]NOT RECORDED IN STRIPE sku=%s reason=settlement returned no transaction id\n",
			sku.SkuId,
		)
		return
	}

	// The network Stripe should look the transaction up on. Prefer what the
	// facilitator said it settled on over what we quoted: if those disagree,
	// the chain the money is actually on is the one that can verify it.
	network := settleResponse.Network
	if network == "" {
		network = requirements.Network
	}
	stripeNetwork, ok := x402StripeNetworks[network]
	if !ok {
		glog.Errorf(
			"[x402]NOT RECORDED IN STRIPE sku=%s tx=%s reason=unmapped network %q\n",
			sku.SkuId, transaction, network,
		)
		return
	}

	amountCents, err := x402StripeAmountCents(requirements.MaxAmountRequired)
	if err != nil {
		glog.Errorf(
			"[x402]NOT RECORDED IN STRIPE sku=%s tx=%s reason=%s\n",
			sku.SkuId, transaction, err,
		)
		return
	}
	if amountCents < 1 {
		// Stripe's minimum is a cent. A sub-cent x402 purchase is possible in
		// principle (the protocol's floor is $0.01, but a future sku could be
		// priced under it), and rounding it up to a cent would book revenue
		// that was never collected.
		glog.Infof(
			"[x402]not recorded in stripe sku=%s tx=%s reason=below one cent\n",
			sku.SkuId, transaction,
		)
		return
	}

	intent, err := x402CreateStripeCryptoPaymentIntent(
		ctx, amountCents, stripeNetwork, transaction, sku.SkuId,
	)
	if err != nil {
		// The one outcome this file exists to make impossible to miss: the
		// customer paid, we delivered, and Stripe does not know. The
		// transaction hash is the thread to pull, and re-running the same call
		// with the same idempotency key is safe.
		glog.Errorf(
			"[x402]NOT RECORDED IN STRIPE sku=%s tx=%s network=%s cents=%d err=%s\n",
			sku.SkuId, transaction, stripeNetwork, amountCents, err,
		)
		return
	}

	glog.Infof(
		"[x402]recorded in stripe sku=%s tx=%s payment_intent=%s status=%s cents=%d\n",
		sku.SkuId, transaction, intent.Id, intent.Status, intent.Amount,
	)
}

// x402StripeAmountCents converts the quoted atomic USDC amount to cents.
//
// The quote is the authority rather than sku.PriceUsd: it is the exact integer
// the agent's payment was bound to, so it is what actually moved. Deriving
// cents from the float price instead would let a rounding difference book a
// different number than the chain shows.
func x402StripeAmountCents(maxAmountRequired string) (int64, error) {
	if maxAmountRequired == "" {
		return 0, fmt.Errorf("terms carried no amount")
	}
	atomic, err := strconv.ParseInt(maxAmountRequired, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q is not an integer of atomic units", maxAmountRequired)
	}
	if atomic < 0 {
		return 0, fmt.Errorf("amount %q is negative", maxAmountRequired)
	}
	// Round to nearest cent rather than truncating, so 1_999_999 atomic units
	// books as $2.00 and not $1.99.
	return (atomic + usdcAtomicPerCent/2) / usdcAtomicPerCent, nil
}

// x402CreateStripeCryptoPaymentIntent creates the PaymentIntent.
//
// Raw HTTP through the stripeApiBaseUrl seam rather than the stripe-go SDK: the
// crypto transaction_verification fields only exist on the preview API version,
// which the pinned SDK does not model.
func x402CreateStripeCryptoPaymentIntent(
	ctx context.Context,
	amountCents int64,
	stripeNetwork string,
	transaction string,
	skuId string,
) (*stripePaymentIntentResult, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(amountCents, 10))
	form.Set("currency", "usd")
	form.Set("confirm", "true")
	form.Set("payment_method_data[type]", "crypto")
	form.Set("allowed_payment_method_types[0]", "crypto")
	form.Set("payment_method_options[crypto][mode]", "transaction_verification")
	form.Set(
		"payment_method_options[crypto][transaction_verification_options][network]",
		stripeNetwork,
	)
	form.Set(
		"payment_method_options[crypto][transaction_verification_options][transaction_hash]",
		transaction,
	)
	// So a payment in Stripe can be traced back to what it bought without
	// joining against our database.
	form.Set("metadata[x402_sku]", skuId)
	form.Set("metadata[x402_transaction]", transaction)

	return server.HttpPostForm[*stripePaymentIntentResult](
		ctx,
		fmt.Sprintf("%s/v1/payment_intents", stripeApiBaseUrl),
		form,
		func(header http.Header) {
			stripeAuthHeader(header)
			header.Set("Stripe-Version", stripePreviewApiVersion)
			// One transfer books once. A retry after a timeout, or a
			// facilitator that settles the same payment twice, must not create
			// a second payment record for the same on-chain transaction.
			header.Set("Idempotency-Key", x402StripeIdempotencyKey(transaction))
		},
		server.HttpResponseRequireStatusOk[*stripePaymentIntentResult](
			server.ResponseJsonObject[*stripePaymentIntentResult],
		),
	)
}

// x402StripeIdempotencyKey namespaces the transaction hash, so an x402
// recording can never collide with an unrelated Stripe call that happened to
// use the same string.
func x402StripeIdempotencyKey(transaction string) string {
	return "x402-record-" + transaction
}
