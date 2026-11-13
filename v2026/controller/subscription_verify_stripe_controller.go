package controller

// Client purchase reporting for Stripe, as subscription_verify_controller.go
// does for Play and the App Store (UPGRADE.md §4 item 2). Before this, a card
// payment credited only through Stripe's webhooks (invoice.paid,
// checkout.session.completed, the payment sheet's setup_intent.succeeded) or
// the hourly payment reconcile: a webhook that came late or never left the
// customer on Free for up to an hour after paying (support inbox 7247).
//
// The app reports what it paid as soon as the checkout or the payment sheet
// finishes: the checkout session id POST /stripe/create-checkout-session
// answered (also the {CHECKOUT_SESSION_ID} of the return url), or the
// subscription id POST /subscription/stripe/payment-sheet answered. The server
// reads it back from Stripe, checks that it belongs to the caller's network,
// and credits it through the same code the webhooks run:
//
//   - a Pro subscription's invoice: stripeHandleInvoicePaidWithOnboarding, the
//     invoice.paid handler, gated by the stripe_invoice ledger, so a report
//     racing the webhook or the reconcile credits exactly once;
//   - a payment sheet's trial, held until the card is attached:
//     stripeFinalizeTierForCustomer, what setup_intent.succeeded runs;
//   - a data pack's checkout: stripeHandleCheckoutSessionCompleted, the
//     checkout.session.completed handler, keyed by the session's purchase
//     event id.
//
// The answer is the verify endpoints' VerifyStorePurchaseResult. credited,
// already_credited, wrong_network and invalid are terminal; pending (Stripe
// has not finished taking the payment) and a non-2xx answer are retried with
// the sdk's PurchaseReportBackoffMillis.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A Stripe object id the report may name: its prefix, then letters, digits
// and underscores. Anything else never reaches Stripe.
var (
	stripeVerifyCheckoutSessionIdPattern = regexp.MustCompile(`^cs_[A-Za-z0-9_]{1,250}$`)
	stripeVerifySubscriptionIdPattern    = regexp.MustCompile(`^sub_[A-Za-z0-9_]{1,250}$`)
)

// The verify endpoint's steps outside Stripe's api. Replaceable only by the
// tests that drive VerifyStripeCheckout without a database; production never
// mutates these.
var (
	stripeVerifyRateLimitFunc        = CheckVerifyPurchaseRateLimit
	stripeVerifyInvoiceNetworkIdFunc = model.GetStripeInvoiceNetworkId
	stripeVerifyCreditInvoiceFunc    = stripeHandleInvoicePaidWithOnboarding
	stripeVerifyFinalizeCustomerFunc = stripeFinalizeTierForCustomer
	// whether a data pack checkout already has its balance code (the
	// purchase event id of its first line)
	stripeVerifyCheckoutCreditedFunc = func(clientSession *session.ClientSession, sessionId string) (bool, error) {
		_, err := model.GetBalanceCodeIdForPurchaseEventId(clientSession.Ctx, stripeCheckoutPurchaseEventId(sessionId, 0))
		return err == nil, nil
	}
	stripeVerifyFulfillCheckoutFunc = stripeHandleCheckoutSessionCompleted
	// the billing country of the card the sheet attached, "" when Stripe
	// cannot say: what setup_intent.succeeded finalizes the tier from
	stripeVerifyPaymentMethodCountry = stripePaymentMethodCountry
	stripeVerifyFetchChargeFunc      = stripeReconcileFetchCharge
	stripeVerifyNowFunc              = server.NowUtc
)

// How a client's report of a card payment is judged.
type stripeVerifySettings struct {
	// A data pack checkout older than this is left to the webhook and
	// support: Stripe retries checkout.session.completed for 72 h, so a report
	// inside that window only brings forward what the webhook would do.
	// Non-positive refuses every data pack report.
	CheckoutMaxAge time.Duration
}

// The settings the endpoint runs with.
func defaultStripeVerifySettings() *stripeVerifySettings {
	return &stripeVerifySettings{
		CheckoutMaxAge: 72 * time.Hour,
	}
}

type VerifyStripeCheckoutArgs struct {
	// the checkout session id POST /stripe/create-checkout-session answered
	// (cs_...), which is also the {CHECKOUT_SESSION_ID} of the return url
	SessionId string `json:"session_id,omitempty"`
	// the subscription id POST /subscription/stripe/payment-sheet answered
	// (sub_...): the inline payment sheet has no checkout session
	SubscriptionId string `json:"subscription_id,omitempty"`
}

// The checkout session as GET /v1/checkout/sessions/{id} answers it. Its
// references (customer, subscription, invoice, payment intent) are ids, or
// objects when expanded.
type stripeVerifyCheckoutSession struct {
	Id                string                                `json:"id"`
	AmountTotal       int                                   `json:"amount_total"`
	Customer          json.RawMessage                       `json:"customer"`
	CustomerDetails   *StripeEventDataObjectCustomerDetails `json:"customer_details"`
	PaymentStatus     string                                `json:"payment_status"`
	ClientReferenceId string                                `json:"client_reference_id"`
	Subscription      json.RawMessage                       `json:"subscription"`
	Metadata          map[string]string                     `json:"metadata"`
	PaymentIntent     json.RawMessage                       `json:"payment_intent,omitempty"`
	// subscription, payment or setup
	Mode string `json:"mode"`
	// open, complete or expired
	Status string `json:"status"`
	// the invoice the session created (subscription mode)
	Invoice json.RawMessage `json:"invoice"`
	// unix seconds
	Created int64 `json:"created"`
}

// The session as checkout.session.completed delivers it, for that event's
// handler.
func (self *stripeVerifyCheckoutSession) completedObject() *StripeEventCheckoutCompleteObject {
	return &StripeEventCheckoutCompleteObject{
		Id:                self.Id,
		AmountTotal:       self.AmountTotal,
		Customer:          stripeIdFromRaw(self.Customer),
		CustomerDetails:   self.CustomerDetails,
		PaymentStatus:     self.PaymentStatus,
		ClientReferenceId: self.ClientReferenceId,
		Subscription:      stripeIdFromRaw(self.Subscription),
		Metadata:          self.Metadata,
		PaymentIntent:     self.PaymentIntent,
	}
}

// A subscription as GET /v1/subscriptions/{id}?expand[]=latest_invoice
// answers it.
type stripeVerifySubscription struct {
	Id                   string            `json:"id"`
	Status               string            `json:"status"`
	Customer             json.RawMessage   `json:"customer"`
	Metadata             map[string]string `json:"metadata"`
	DefaultPaymentMethod json.RawMessage   `json:"default_payment_method"`
	LatestInvoice        json.RawMessage   `json:"latest_invoice"`
}

// An invoice as GET /v1/invoices/{id} answers it: what invoice.paid delivers,
// its status, and the period its first line pays for.
type stripeVerifyInvoice struct {
	Id            string          `json:"id"`
	Total         int             `json:"total"`
	Customer      json.RawMessage `json:"customer"`
	Currency      string          `json:"currency"`
	BillingReason string          `json:"billing_reason,omitempty"`
	// draft, open, paid, uncollectible or void
	Status string `json:"status"`
	// in the currency's minor unit; 0 for a trial or a fully discounted invoice
	AmountPaid int64 `json:"amount_paid"`
	// the charge that paid it, when the api version names it
	Charge json.RawMessage `json:"charge"`
	Lines  *struct {
		Data []*struct {
			Period *struct {
				End int64 `json:"end"`
			} `json:"period"`
		} `json:"data"`
	} `json:"lines"`
}

// The invoice as invoice.paid delivers it, for that event's handler.
func (self *stripeVerifyInvoice) eventObject() *StripeEventInvoiceObject {
	return &StripeEventInvoiceObject{
		Id:            self.Id,
		Total:         self.Total,
		Customer:      stripeIdFromRaw(self.Customer),
		Currency:      self.Currency,
		BillingReason: self.BillingReason,
	}
}

// The end of the period the invoice pays for, nil when the invoice names
// none.
func (self *stripeVerifyInvoice) paidThrough() *time.Time {
	if self.Lines == nil {
		return nil
	}
	for _, line := range self.Lines.Data {
		if line != nil && line.Period != nil && 0 < line.Period.End {
			end := time.Unix(line.Period.End, 0).UTC()
			return &end
		}
	}
	return nil
}

// Answers POST /subscription/verify-stripe-checkout: a client's report of a
// card payment, read back from Stripe and credited through the webhooks' own
// code.
//
// decision table (Stripe's state -> status):
//
//	no id, or a malformed one                                 -> invalid
//	the session or subscription is unknown to Stripe (404)    -> invalid
//	it names no network (no client_reference_id / network_id) -> invalid
//	it names another network                                  -> wrong_network
//	checkout open, or complete and not yet paid               -> pending
//	checkout expired, setup mode                              -> invalid
//	subscription incomplete, its invoice open or draft        -> pending
//	subscription incomplete_expired or canceled unpaid,
//	  its invoice void or uncollectible                       -> invalid
//	a payment sheet's trial whose card is not attached yet    -> pending
//	paid and this report landed the credit                    -> credited
//	paid and the credit was there (webhook, reconcile, report)-> already_credited
//	Stripe api or database failure                            -> error (non-2xx, retry)
func VerifyStripeCheckout(
	args *VerifyStripeCheckoutArgs,
	clientSession *session.ClientSession,
) (*VerifyStorePurchaseResult, error) {
	// cheap validation first, so malformed requests burn no rate-limit budget
	// and no Stripe api calls
	sessionId := strings.TrimSpace(args.SessionId)
	subscriptionId := strings.TrimSpace(args.SubscriptionId)
	switch {
	case sessionId != "" && subscriptionId == "":
		if !stripeVerifyCheckoutSessionIdPattern.MatchString(sessionId) {
			return NewVerifyStorePurchaseInvalid(), nil
		}
	case sessionId == "" && subscriptionId != "":
		if !stripeVerifySubscriptionIdPattern.MatchString(subscriptionId) {
			return NewVerifyStorePurchaseInvalid(), nil
		}
	default:
		return NewVerifyStorePurchaseInvalid(), nil
	}

	if err := stripeVerifyRateLimitFunc(clientSession); err != nil {
		return nil, err
	}

	if sessionId != "" {
		return verifyStripeCheckoutSession(sessionId, clientSession, defaultStripeVerifySettings())
	}
	return verifyStripePaymentSheetSubscription(subscriptionId, clientSession)
}

// A report of a checkout session: Pro (subscription mode) or a data pack
// (payment mode).
func verifyStripeCheckoutSession(
	sessionId string,
	clientSession *session.ClientSession,
	settings *stripeVerifySettings,
) (*VerifyStorePurchaseResult, error) {
	checkoutSession, found, err := stripeVerifyGet[*stripeVerifyCheckoutSession](
		clientSession,
		fmt.Sprintf("%s/v1/checkout/sessions/%s", stripeApiBaseUrl, url.PathEscape(sessionId)),
	)
	if err != nil {
		return nil, err
	}
	if !found || checkoutSession == nil || checkoutSession.Id != sessionId {
		return NewVerifyStorePurchaseInvalid(), nil
	}

	// every checkout this server starts names the network it is for
	// (StripeCreateCheckoutSession, /pay/data/checkout for a named network)
	if checkoutSession.ClientReferenceId == "" {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	networkId, err := server.ParseId(checkoutSession.ClientReferenceId)
	if err != nil {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	if networkId != clientSession.ByJwt.NetworkId {
		glog.Infof(
			"[sub]verify stripe checkout %s: it is for network %s, session is %s\n",
			sessionId, networkId, clientSession.ByJwt.NetworkId,
		)
		return verifyStatusResult(VerifyPurchaseStatusWrongNetwork), nil
	}

	switch checkoutSession.Status {
	case "open":
		// the customer has not finished the checkout
		return verifyStatusResult(VerifyPurchaseStatusPending), nil
	case "complete":
	default:
		// expired, or a status this server does not know
		return NewVerifyStorePurchaseInvalid(), nil
	}

	switch checkoutSession.Mode {
	case "subscription":
		invoiceId := stripeIdFromRaw(checkoutSession.Invoice)
		if invoiceId == "" {
			// an api version that does not name the session's invoice: the
			// subscription's latest invoice is the one the checkout created
			// (Pro checkouts are never reported a whole period later)
			subscriptionId := stripeIdFromRaw(checkoutSession.Subscription)
			if subscriptionId == "" {
				return verifyStatusResult(VerifyPurchaseStatusPending), nil
			}
			subscription, found, err := stripeVerifyReadSubscription(clientSession, subscriptionId)
			if err != nil {
				return nil, err
			}
			if !found {
				return NewVerifyStorePurchaseInvalid(), nil
			}
			invoiceId = stripeIdFromRaw(subscription.LatestInvoice)
			if invoiceId == "" {
				return verifyStatusResult(VerifyPurchaseStatusPending), nil
			}
		}
		return verifyStripeInvoice(invoiceId, clientSession)

	case "payment":
		// a data pack: only once its money is in (a delayed payment method
		// completes the session unpaid)
		if checkoutSession.PaymentStatus != "paid" {
			return verifyStatusResult(VerifyPurchaseStatusPending), nil
		}
		fulfilled, err := stripeVerifyCheckoutCreditedFunc(clientSession, sessionId)
		if err != nil {
			return nil, err
		}
		if fulfilled {
			return verifyStatusResult(VerifyPurchaseStatusAlreadyCredited), nil
		}
		if created := time.Unix(checkoutSession.Created, 0); checkoutSession.Created <= 0 || settings.CheckoutMaxAge < stripeVerifyNowFunc().Sub(created) {
			return NewVerifyStorePurchaseInvalid(), nil
		}
		if _, err := stripeVerifyFulfillCheckoutFunc(checkoutSession.completedObject(), clientSession); err != nil {
			return nil, err
		}
		return verifyStatusResult(VerifyPurchaseStatusCredited), nil

	default:
		// setup mode buys nothing
		return NewVerifyStorePurchaseInvalid(), nil
	}
}

// A report of the payment sheet's subscription, which has no checkout
// session.
func verifyStripePaymentSheetSubscription(
	subscriptionId string,
	clientSession *session.ClientSession,
) (*VerifyStorePurchaseResult, error) {
	subscription, found, err := stripeVerifyReadSubscription(clientSession, subscriptionId)
	if err != nil {
		return nil, err
	}
	if !found || subscription.Id != subscriptionId {
		return NewVerifyStorePurchaseInvalid(), nil
	}

	// the payment sheet stamps the network on the subscription
	// (StripePaymentSheet)
	networkIdText := subscription.Metadata[stripeMetadataNetworkId]
	if networkIdText == "" {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	networkId, err := server.ParseId(networkIdText)
	if err != nil {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	if networkId != clientSession.ByJwt.NetworkId {
		glog.Infof(
			"[sub]verify stripe subscription %s: it is for network %s, session is %s\n",
			subscriptionId, networkId, clientSession.ByJwt.NetworkId,
		)
		return verifyStatusResult(VerifyPurchaseStatusWrongNetwork), nil
	}

	invoiceId := stripeIdFromRaw(subscription.LatestInvoice)
	switch subscription.Status {
	case "incomplete":
		// the sheet has not confirmed the first payment yet
		return verifyStatusResult(VerifyPurchaseStatusPending), nil
	case "active", "trialing", "past_due", "unpaid", "paused":
	case "canceled":
		// a cancel can come with a refund: a report never credits a canceled
		// subscription, and only says whether its last invoice was credited
		if invoiceId != "" {
			if result, done := stripeVerifyLedgerStatus(clientSession, invoiceId, nil); done {
				return result, nil
			}
		}
		return NewVerifyStorePurchaseInvalid(), nil
	default:
		// incomplete_expired: the sheet was abandoned, nothing was charged
		return NewVerifyStorePurchaseInvalid(), nil
	}
	if invoiceId == "" {
		return verifyStatusResult(VerifyPurchaseStatusPending), nil
	}

	if subscription.Metadata[stripeMetadataPaymentSheet] == stripePaymentSheetPending && subscription.Status == "trialing" {
		// the $0 trial invoice is held until the sheet attaches the card
		// (stripeHandleInvoicePaidWithOnboarding). With the card on the
		// subscription, finalize as setup_intent.succeeded does: the tier
		// from the card's country, then the held invoice's credit.
		paymentMethodId := stripeIdFromRaw(subscription.DefaultPaymentMethod)
		if paymentMethodId == "" {
			return verifyStatusResult(VerifyPurchaseStatusPending), nil
		}
		customerId := stripeIdFromRaw(subscription.Customer)
		if customerId == "" {
			return NewVerifyStorePurchaseInvalid(), nil
		}
		if result, done := stripeVerifyLedgerStatus(clientSession, invoiceId, nil); done {
			return result, nil
		}
		// the finalization reads and updates the customer's subscriptions
		// through the stripe-go sdk
		stripe.Key = stripeApiToken()
		stripeVerifyFinalizeCustomerFunc(clientSession, customerId, stripeVerifyPaymentMethodCountry(paymentMethodId))
		return stripeVerifyInvoiceCreditStatus(clientSession, invoiceId, nil)
	}

	return verifyStripeInvoice(invoiceId, clientSession)
}

// Credits a paid invoice of the caller's subscription through the
// invoice.paid handler, once.
func verifyStripeInvoice(
	invoiceId string,
	clientSession *session.ClientSession,
) (*VerifyStorePurchaseResult, error) {
	invoice, found, err := stripeVerifyGet[*stripeVerifyInvoice](
		clientSession,
		fmt.Sprintf("%s/v1/invoices/%s", stripeApiBaseUrl, url.PathEscape(invoiceId)),
	)
	if err != nil {
		return nil, err
	}
	if !found || invoice == nil || invoice.Id != invoiceId {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	switch invoice.Status {
	case "paid":
	case "draft", "open":
		// Stripe is still taking the payment
		return verifyStatusResult(VerifyPurchaseStatusPending), nil
	default:
		// void or uncollectible: it was never paid
		return NewVerifyStorePurchaseInvalid(), nil
	}
	paidThrough := invoice.paidThrough()

	if result, done := stripeVerifyLedgerStatus(clientSession, invoiceId, paidThrough); done {
		return result, nil
	}

	// Stripe keeps a refunded or disputed invoice "paid": only its charge
	// tells. A report never credits money that went back; the webhooks and
	// the reconcile still judge such an invoice as they always have.
	if chargeId := stripeIdFromRaw(invoice.Charge); chargeId != "" && 0 < invoice.AmountPaid {
		charge, err := stripeVerifyFetchChargeFunc(clientSession.Ctx, chargeId)
		if err != nil {
			return nil, err
		}
		if !charge.keptWholePayment() {
			glog.Infof("[sub]verify stripe invoice %s: its charge %s gave money back; not credited by the report\n", invoiceId, chargeId)
			return NewVerifyStorePurchaseInvalid(), nil
		}
	}

	// a renewal Stripe retried and was paid now resolves its failure, as on
	// invoice.paid (subscription_stripe_dunning.go)
	stripeResolveInvoicePaymentFailureFunc(
		clientSession.Ctx,
		invoice.Id,
		model.StripeInvoicePaymentFailureResolvePaid,
		stripeDunningNow(),
	)
	if _, err := stripeVerifyCreditInvoiceFunc(invoice.eventObject(), clientSession); err != nil {
		var unresolved *stripeInvoiceDestinationUnresolvedError
		if errors.As(err, &unresolved) {
			// the invoice names no network; the webhook records it for support
			return NewVerifyStorePurchaseInvalid(), nil
		}
		return nil, err
	}
	return stripeVerifyInvoiceCreditStatus(clientSession, invoiceId, paidThrough)
}

// The answer for an invoice the ledger already holds: already_credited for
// the caller's network, wrong_network for another. done = false when the
// ledger does not hold it.
func stripeVerifyLedgerStatus(
	clientSession *session.ClientSession,
	invoiceId string,
	paidThrough *time.Time,
) (result *VerifyStorePurchaseResult, done bool) {
	creditedNetworkId, credited := stripeVerifyInvoiceNetworkIdFunc(clientSession.Ctx, invoiceId)
	if !credited {
		return nil, false
	}
	if creditedNetworkId != clientSession.ByJwt.NetworkId {
		return verifyStatusResult(VerifyPurchaseStatusWrongNetwork), true
	}
	return &VerifyStorePurchaseResult{
		Status:     VerifyPurchaseStatusAlreadyCredited,
		ExpiryTime: paidThrough,
	}, true
}

// Reads the ledger after a credit attempt on an invoice it did not hold
// before: credited when the caller's network holds the invoice now (this
// report, or a webhook in the same moment, landed it), pending when nothing
// credited it (a payment sheet trial whose finalization did not get to it).
func stripeVerifyInvoiceCreditStatus(
	clientSession *session.ClientSession,
	invoiceId string,
	paidThrough *time.Time,
) (*VerifyStorePurchaseResult, error) {
	creditedNetworkId, credited := stripeVerifyInvoiceNetworkIdFunc(clientSession.Ctx, invoiceId)
	if !credited {
		return verifyStatusResult(VerifyPurchaseStatusPending), nil
	}
	if creditedNetworkId != clientSession.ByJwt.NetworkId {
		return verifyStatusResult(VerifyPurchaseStatusWrongNetwork), nil
	}
	return &VerifyStorePurchaseResult{Status: VerifyPurchaseStatusCredited, ExpiryTime: paidThrough}, nil
}

// A subscription with its latest invoice, as the payment sheet created it.
func stripeVerifyReadSubscription(
	clientSession *session.ClientSession,
	subscriptionId string,
) (*stripeVerifySubscription, bool, error) {
	subscription, found, err := stripeVerifyGet[*stripeVerifySubscription](
		clientSession,
		fmt.Sprintf("%s/v1/subscriptions/%s?expand[]=latest_invoice", stripeApiBaseUrl, url.PathEscape(subscriptionId)),
	)
	if err != nil || !found || subscription == nil {
		return nil, false, err
	}
	return subscription, true, nil
}

// Reads one object from Stripe's api. found = false when Stripe does not know
// it (400, 404), which no retry changes.
func stripeVerifyGet[R any](clientSession *session.ClientSession, objectUrl string) (result R, found bool, returnErr error) {
	result, err := server.HttpGetRequireStatusOk[R](
		clientSession.Ctx,
		objectUrl,
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
		},
		server.ResponseJsonObject[R],
	)
	if err != nil {
		var statusErr *server.HttpStatusError
		if errors.As(err, &statusErr) {
			switch statusErr.StatusCode {
			case http.StatusBadRequest, http.StatusNotFound:
				return result, false, nil
			}
		}
		// credentials or Stripe trouble: retryable, non-2xx
		return result, false, err
	}
	return result, true, nil
}
