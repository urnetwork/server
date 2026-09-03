package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/ephemeralkey"

	stripesession "github.com/stripe/stripe-go/v82/billingportal/session"
	stripecheckout "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/subscription"
	stripewebhook "github.com/stripe/stripe-go/v82/webhook"
)

var stripeWebhookSigningSecret = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["webhook"].(map[string]any)["signing_secret"].(string)
})

var stripeApiToken = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["api"].(map[string]any)["token"].(string)
})

var stripePublishableKey = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["api"].(map[string]any)["publishable_key"].(string)
})

var stripeSkus = sync.OnceValue(func() map[string]*Sku {
	var skus Skus
	server.Config.RequireSimpleResource("stripe.yml").UnmarshalYaml(&skus)
	return skus.Skus
})

type StripeSubscriptionPrices struct {
	Yearly  string `yaml:"yearly"`
	Monthly string `yaml:"monthly"`
}

// The Stripe price ids for the Pro plan. These live in config/<env>/stripe.yml, NOT
// the vault -- a price id is not a secret.
//
// This used to read from the Vault (where `subscription_prices` does not exist) and
// then type-assert a map[string]any straight to this struct, which would have panicked
// the moment it was called. It never was called, so the bug sat here unnoticed.
var stripeSubscriptionPrices = sync.OnceValue(func() StripeSubscriptionPrices {
	var c struct {
		SubscriptionPrices StripeSubscriptionPrices `yaml:"subscription_prices"`
	}
	// optional: an env with no config stripe.yml (local, test) simply has no prices,
	// and checkout refuses rather than the process failing to boot
	if resource, err := server.Config.SimpleResource("stripe.yml"); err == nil {
		resource.UnmarshalYaml(&c)
	}
	return c.SubscriptionPrices
})

// Where Stripe returns the customer after checkout. Configured SERVER-side on purpose:
// if the client could supply success_url, anyone could point our checkout at their own
// domain and harvest the redirect.
//
// success_url/cancel_url are the HOSTED pair. return_url is the EMBEDDED one: Stripe
// rejects success_url/cancel_url when ui_mode is embedded, and takes a single return_url
// instead (it substitutes {CHECKOUT_SESSION_ID} into it).
type StripeCheckoutUrls struct {
	SuccessUrl string `yaml:"success_url"`
	CancelUrl  string `yaml:"cancel_url"`
	ReturnUrl  string `yaml:"return_url"`
}

var stripeCheckoutUrls = sync.OnceValue(func() StripeCheckoutUrls {
	var c struct {
		Checkout StripeCheckoutUrls `yaml:"checkout"`
	}
	// optional, as above. StripeCreateCheckoutSession refuses to send a customer to
	// Stripe when these are unset, rather than sending them somewhere with no way back.
	if resource, err := server.Config.SimpleResource("stripe.yml"); err == nil {
		resource.UnmarshalYaml(&c)
	}
	return c.Checkout
})

type StripeWebhookArgs struct {
	Id   string           `json:"id"`
	Type string           `json:"type"`
	Data *StripeEventData `json:"data"`
}

type StripeEventData struct {
	Object json.RawMessage `json:"object"`
}

type StripeEventCheckoutCompleteObject struct {
	Id                string                                `json:"id"`
	AmountTotal       int                                   `json:"amount_total"`
	Customer          string                                `json:"customer"`
	CustomerDetails   *StripeEventDataObjectCustomerDetails `json:"customer_details"`
	PaymentStatus     string                                `json:"payment_status"`
	ClientReferenceId string                                `json:"client_reference_id"`
	Subscription      string                                `json:"subscription"`
	// set by /pay/data/checkout (apply_to_network, network_id, network_name, item_id)
	Metadata map[string]string `json:"metadata"`
}

// stripeAppliedNetworkName is the network a buy-data checkout was made FOR, from
// the session metadata; empty for every other checkout. The network id itself
// still comes from client_reference_id, which is what the fulfilment uses.
func stripeAppliedNetworkName(checkoutComplete *StripeEventCheckoutCompleteObject) string {
	if checkoutComplete.Metadata == nil {
		return ""
	}
	if checkoutComplete.Metadata[payDataMetadataApplyToNetwork] != payDataMetadataApplyYes {
		return ""
	}
	if checkoutComplete.ClientReferenceId == "" {
		return ""
	}
	return checkoutComplete.Metadata[payDataMetadataNetworkName]
}

type StripeEventInvoiceObject struct {
	Id          string `json:"id"`
	Total       int    `json:"total"`
	PeriodStart int    `json:"period_start"`
	PeriodEnd   int    `json:"period_end"`
	Customer    string `json:"customer"`
}

type StripeEventDataObjectCustomerDetails struct {
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
}

// The refund/dispute event objects (UPGRADE.md §2 S7). In webhook payloads
// the invoice / payment_intent / charge references are id strings (never
// expanded objects), and null decodes to "".

type StripeEventRefundObject struct {
	Id            string `json:"id"`
	Charge        string `json:"charge"`
	PaymentIntent string `json:"payment_intent"`
	Amount        int    `json:"amount"`
}

type StripeEventChargeRefundList struct {
	Data []*StripeEventRefundObject `json:"data"`
}

type StripeEventChargeObject struct {
	Id             string `json:"id"`
	Invoice        string `json:"invoice"`
	PaymentIntent  string `json:"payment_intent"`
	AmountRefunded int    `json:"amount_refunded"`
	// embedded on older API versions' charge.refunded payloads; newer
	// versions omit it and the refunds are listed by charge instead
	Refunds *StripeEventChargeRefundList `json:"refunds"`
}

type StripeEventDisputeObject struct {
	Id            string `json:"id"`
	Charge        string `json:"charge"`
	PaymentIntent string `json:"payment_intent"`
	Amount        int    `json:"amount"`
}

type StripeWebhookResultErr struct {
	Message string `json:"message"`
}

type StripeWebhookResult struct {
	Error *StripeWebhookResultErr `json:"message,omitempty"`
}

type StripeLineItems struct {
	Data []*StripeLineItem `json:"data"`
}

type StripeSubscription struct {
	Id   string                    `json:"id"`
	Data []*StripeSubscriptionItem `json:"data"`
}

type StripeSubscriptionItem struct {
	Id                 string `json:"id"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
}

type StripeLineItem struct {
	Id           string                 `json:"id"`
	AmountTotal  int                    `json:"amount_total"`
	Currency     string                 `json:"currency"`
	Description  string                 `json:"description"`
	Price        *StripeLineItemProduct `json:"price"`
	Quantity     int                    `json:"quantity"`
	Type         string                 `json:"type"`
	Subscription *string                `json:"subscription,omitempty"`
	Period       *StripeLineItemPeriod  `json:"period,omitempty"`
}

type StripeLineItemPeriod struct {
	End   int64 `json:"end"`
	Start int64 `json:"start"`
}

type StripeLineItemProduct struct {
	Id         string `json:"id"`
	Product    string `json:"product"`
	UnitAmount int    `json:"unit_amount"`
}

func VerifyStripeBody(req *http.Request) (io.Reader, error) {
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	_, err = stripewebhook.ConstructEventWithOptions(
		bodyBytes,
		req.Header.Get("Stripe-Signature"),
		stripeWebhookSigningSecret(),
		stripewebhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true},
	)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(bodyBytes), nil
}

func StripeWebhook(
	stripeWebhook *StripeWebhookArgs,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {

	glog.Infof("Stripe webhook hit: %s", stripeWebhook.Id)

	if stripeWebhook.Type == "checkout.session.completed" {

		/**
		 * need to check, but I think this is deprecated?
		 * used for topping up individual transfer balances, which we don't have in the app atm
		 */

		glog.Infof("type: checkout.session.completed")

		var checkoutCompleteObject StripeEventCheckoutCompleteObject
		if err := json.Unmarshal(stripeWebhook.Data.Object, &checkoutCompleteObject); err != nil {
			glog.Errorf("Failed to parse checkout session: %v", err)
			return nil, fmt.Errorf("failed to parse invoice: %v", err)
		}

		return stripeHandleCheckoutSessionCompleted(
			&checkoutCompleteObject,
			clientSession,
		)

	} else if stripeWebhook.Type == "invoice.paid" {

		/**
		 * new user subscription
		 */

		glog.Infof("type: invoice.paid")

		var invoiceObject StripeEventInvoiceObject
		if err := json.Unmarshal(stripeWebhook.Data.Object, &invoiceObject); err != nil {
			return nil, fmt.Errorf("failed to parse invoice: %v", err)
		}

		return stripeHandleInvoicePaid(
			&invoiceObject,
			clientSession,
		)

	} else if stripeWebhook.Type == "charge.refunded" && stripeWebhook.Data != nil {

		/**
		 * the store returned the money -- claw back what the charge granted (S7)
		 */

		glog.Infof("type: charge.refunded")

		var chargeObject StripeEventChargeObject
		if err := json.Unmarshal(stripeWebhook.Data.Object, &chargeObject); err != nil {
			return nil, fmt.Errorf("failed to parse charge: %v", err)
		}

		return stripeHandleChargeRefunded(&chargeObject, clientSession)

	} else if stripeWebhook.Type == "refund.created" && stripeWebhook.Data != nil {

		glog.Infof("type: refund.created")

		var refundObject StripeEventRefundObject
		if err := json.Unmarshal(stripeWebhook.Data.Object, &refundObject); err != nil {
			return nil, fmt.Errorf("failed to parse refund: %v", err)
		}

		return stripeHandleRefundCreated(&refundObject, clientSession)

	} else if stripeWebhook.Type == "charge.dispute.created" && stripeWebhook.Data != nil {

		/**
		 * a dispute withdraws the funds immediately -- treat like a refund,
		 * with its own action label
		 */

		glog.Infof("type: charge.dispute.created")

		var disputeObject StripeEventDisputeObject
		if err := json.Unmarshal(stripeWebhook.Data.Object, &disputeObject); err != nil {
			return nil, fmt.Errorf("failed to parse dispute: %v", err)
		}

		return stripeHandleDisputeCreated(&disputeObject, clientSession)

	}
	// else IGNORE the event and answer 200. This is load-bearing: the endpoint
	// subscribes to the full event catalog, and a non-2xx on an unhandled type
	// would make Stripe retry it for 72h and eventually DISABLE the endpoint --
	// taking every crediting webhook down with it.

	return &StripeWebhookResult{}, nil
}

func stripeHandleCheckoutSessionCompleted(
	checkoutComplete *StripeEventCheckoutCompleteObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {

	glog.Infof("Stripe stripeHandleCheckoutSessionCompleted")

	stripeSessionId := checkoutComplete.Id

	// need to make a second call to get the line items for the order
	// https://stripe.com/docs/api/checkout/sessions/line_items
	url := fmt.Sprintf(
		stripeApiBaseUrl+"/v1/checkout/sessions/%s/line_items",
		stripeSessionId,
	)
	lineItems, err := server.HttpGetRequireStatusOk[*StripeLineItems](
		clientSession.Ctx,
		url,
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
		},
		server.ResponseJsonObject[*StripeLineItems],
	)
	if err != nil {
		glog.Errorf("Failed to fetch line items: %v", err)
		return nil, err
	}

	// The checkout session we created carries client_reference_id = the network of the
	// signed-in customer. Use it: we know exactly whose balance this is, so the data can
	// land directly instead of being emailed as a code for them to redeem by hand.
	//
	// It is absent for any checkout not started from a signed-in session, in which case
	// the emailed code is the delivery mechanism -- which is what data codes are for.
	var redeemNetworkId *server.Id
	if checkoutComplete.ClientReferenceId != "" {
		if networkId, err := server.ParseId(checkoutComplete.ClientReferenceId); err == nil {
			redeemNetworkId = &networkId
		} else {
			glog.Errorf(
				"[sub]checkout %s has an unparseable client_reference_id %q: %s\n",
				checkoutComplete.Id, checkoutComplete.ClientReferenceId, err,
			)
		}
	}

	// customer_details is absent on some completed sessions, so guard the
	// deref: a nil here would panic the webhook handler rather than return
	purchaseEmail := ""
	if checkoutComplete.CustomerDetails != nil {
		purchaseEmail = checkoutComplete.CustomerDetails.Email
	}
	// No email is only fatal when we ALSO do not know the network: with a known
	// network the credit lands directly and the emailed code is just the record.
	// Erroring out here regardless meant a paid session with a known network was
	// never fulfilled once Stripe's retry window (72h) lapsed.
	if purchaseEmail == "" && redeemNetworkId == nil {
		glog.Infof("missing purchase email")
		return nil, errors.New("missing purchase email to send balance code")
	}

	// a buy-data checkout made FOR a network: the credit lands there and the
	// email says so, instead of offering a code
	appliedNetworkName := ""
	if redeemNetworkId != nil {
		appliedNetworkName = stripeAppliedNetworkName(checkoutComplete)
	}

	if err := stripeFulfillCheckoutLineItems(
		clientSession.Ctx,
		stripeSessionId,
		lineItems.Data,
		purchaseEmail,
		redeemNetworkId,
		appliedNetworkName,
	); err != nil {
		return nil, err
	}

	return &StripeWebhookResult{}, nil

}

// stripeCheckoutPurchaseEventId keys a balance code to ONE line of ONE checkout
// session. The first line keeps the bare session id -- the key every
// already-fulfilled session in the ledger was written under, so a late Stripe
// retry of an old session stays idempotent -- and later lines get their index
// appended, so a multi-line session fulfils every line instead of only the first.
func stripeCheckoutPurchaseEventId(stripeSessionId string, lineIndex int) string {
	if lineIndex == 0 {
		return stripeSessionId
	}
	return fmt.Sprintf("%s/%d", stripeSessionId, lineIndex)
}

// stripeSkusFunc is replaceable only by the hermetic checkout fulfillment tests
// (this test env has no config stripe.yml to read skus from). Production never
// mutates it.
var stripeSkusFunc = func() map[string]*Sku { return stripeSkus() }

// stripeApiBaseUrl / stripeApiTokenFunc are the seams for every raw-HTTP call
// to the Stripe API (the stripe-go SDK calls are separate). Replaceable only
// by hermetic tests standing up a fake Stripe backend (this test env has no
// vault stripe.yml to read the real token from) -- the S5 Play seam pattern.
// Production never mutates them.
var stripeApiBaseUrl = "https://api.stripe.com"
var stripeApiTokenFunc = func() string { return stripeApiToken() }

// stripeFulfillCheckoutLineItems fulfils EVERY line of a completed checkout
// session, quantity included. Factored out of the webhook handler so the
// per-line keying and quantity arithmetic are testable without Stripe.
func stripeFulfillCheckoutLineItems(
	ctx context.Context,
	stripeSessionId string,
	lineItems []*StripeLineItem,
	purchaseEmail string,
	redeemNetworkId *server.Id,
	appliedNetworkName string,
) error {
	skus := stripeSkusFunc()
	for lineIndex, lineItem := range lineItems {
		stripeSku := lineItem.Price.Product
		if sku, ok := skus[stripeSku]; ok {

			glog.Infof("Stripe stripeHandleCheckoutSessionCompleted")

			if sku.Supporter {

				// do nothing
				// handle subscription in `invoid.paid` event

			} else {

				/**
				 * Otherwise user is purchasing balance
				 */

				stripeItemJsonBytes, err := json.Marshal(lineItem)
				if err != nil {
					return err
				}

				// AmountTotal is the LINE total, so revenue already includes quantity
				netRevenue := model.UsdToNanoCents((1.0 - sku.FeeFraction) * float64(lineItem.AmountTotal) / 100.0)

				// the data must scale with quantity too: two units on one line were
				// paid for twice and used to deliver once
				quantity := model.ByteCount(lineItem.Quantity)
				if quantity <= 0 {
					quantity = 1
				}

				glog.Infof("[sub]create balance code: %s %s\n", purchaseEmail, string(stripeItemJsonBytes))

				err = createBalanceCode(
					ctx,
					quantity*sku.BalanceByteCount(),
					model.Pro().DataCodeDuration,
					netRevenue,
					stripeCheckoutPurchaseEventId(stripeSessionId, lineIndex),
					string(stripeItemJsonBytes),
					purchaseEmail,
					redeemNetworkId,
					appliedNetworkName,
				)
				if err != nil {
					return err
				}

			}

		} else {
			glog.Infof("Stripe sku not found: %s", stripeSku)
			return fmt.Errorf("Stripe sku not found: %s", stripeSku)
		}
	}

	return nil
}

type StripeCustomerExpanded struct {
	Id    string `json:"id"`
	Email string `json:"email"`
}

type StripeInvoiceExpanded struct {
	Customer     *StripeCustomerExpanded `json:"customer"`
	Lines        *StripeLineItems        `json:"lines"`
	Subscription *struct {
		ID       string            `json:"id"`
		Metadata map[string]string `json:"metadata"`
	} `json:"subscription"`
}

func stripeHandleInvoicePaid(
	invoice *StripeEventInvoiceObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {

	invoiceId := invoice.Id
	total := invoice.Total

	url := fmt.Sprintf("%s/v1/invoices/%s?expand[]=subscription&expand[]=customer", stripeApiBaseUrl, invoiceId)
	fullInvoice, err := server.HttpGetRequireStatusOk[*StripeInvoiceExpanded](
		clientSession.Ctx,
		url,
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
		},
		server.ResponseJsonObject[*StripeInvoiceExpanded],
	)
	if err != nil {
		glog.Errorf("Failed to fetch invoice details: %v", err)
		return nil, fmt.Errorf("failed to fetch invoice details: %v", err)
	}

	var subscriptionId string
	var periodStart int64
	var periodEnd int64
	isSubscription := false

	if fullInvoice.Lines != nil {
		if len(fullInvoice.Lines.Data) > 0 {

			if lineItem := fullInvoice.Lines.Data[0]; lineItem != nil {
				// Check if this is a subscription line item
				// if itemType, ok := lineItem["type"].(string); ok && itemType == "subscription" {
				if itemType := lineItem.Type; itemType == "subscription" {
					isSubscription = true

					if lineItem.Subscription != nil && *lineItem.Subscription != "" {
						subscriptionId = *lineItem.Subscription
					}

					// Extract period information
					if period := lineItem.Period; period != nil {
						periodStart = period.Start
						periodEnd = period.End
					}
				} else {
					glog.Infof("Line item is not a subscription type: %s", itemType)
				}
			}
		} else {
			glog.Infof("No line items found in invoice")
		}
	} else {
		glog.Infof("No lines object found in invoice")
	}

	if !isSubscription {
		glog.Infof("Invoice is not for a subscription")
		return &StripeWebhookResult{}, nil
	}

	if subscriptionId == "" {
		glog.Infof("Invoice does not have a subscription in its line items")
		return &StripeWebhookResult{}, nil
	}

	var networkId *server.Id

	// check subscription metadata for network id
	if fullInvoice.Subscription != nil {

		glog.Infof("checking subscription metadata for network id")

		glog.Infof("subscription metadata: %v", fullInvoice.Subscription.Metadata)

		if subNetworkId := fullInvoice.Subscription.Metadata["network_id"]; subNetworkId != "" {

			glog.Infof("found network id in subscription metadata: %s", subNetworkId)

			id, err := server.ParseId(subNetworkId)
			if err != nil {
				glog.Infof("Invalid network id format in subscription metadata: %v", err)

				// todo - we should store in the DB webhook errors table
				return nil, fmt.Errorf("failed to parse id err=%v id=%s", err, subNetworkId)

			} else {
				glog.Infof("found network id in subscription metadata: %s", id.String())
				networkId = &id
			}
		} else {
			glog.Infof("no network id in subscription metadata")
		}
	}

	if networkId == nil {

		glog.Infof("no network id in subscription metadata, checking checkout session")

		// check for client_reference_id in the checkout session. This is checked
		// BEFORE the email fallback: the session's client_reference_id names the
		// exact network the checkout was started from, while an email is only a
		// guess (see below).

		// Get the checkout session using the subscription ID
		url = fmt.Sprintf("%s/v1/checkout/sessions?subscription=%s", stripeApiBaseUrl, subscriptionId)
		sessionsResp, err := server.HttpGetRequireStatusOk[map[string]interface{}](
			clientSession.Ctx,
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
			},
			server.ResponseJsonObject[map[string]interface{}],
		)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch checkout session: %v", err)
		}

		if dataArray, ok := sessionsResp["data"].([]interface{}); ok && len(dataArray) > 0 {
			if session, ok := dataArray[0].(map[string]interface{}); ok {
				if clientRefId, ok := session["client_reference_id"].(string); ok {
					id, err := server.ParseId(clientRefId)
					if err != nil {
						return nil, fmt.Errorf("failed to parse id err=%v id=%s", err, clientRefId)
					}
					networkId = &id
				}
			}
		}
	}

	// LAST resort: associate by the Stripe customer's email. This exists only for
	// legacy subscriptions created before the network id was stamped into the
	// subscription metadata, and it can be WRONG: a customer paying with an email
	// that matches a different account credits that account. Kept (loudly) until
	// the legacy subscriptions all carry metadata, then it should be retired.
	// Every use that actually credits is ALSO recorded as an email_fallback
	// operator event below (S11), surfaced by the stripe reconciler leg and the
	// `bringyourctl payments reconcile` summary.
	emailFallback := false
	if networkId == nil && fullInvoice != nil && fullInvoice.Customer != nil && fullInvoice.Customer.Email != "" {

		glog.Warningf(
			"[sub]invoice %s resolving network by customer email fallback -- "+
				"legacy subscription without metadata\n",
			invoiceId,
		)

		// search network by email
		foundId, err := model.FindNetworkIdByEmail(clientSession.Ctx, fullInvoice.Customer.Email)

		if err != nil {
			return nil, fmt.Errorf("failed to find network by email: %v", err)
		}

		if foundId != nil {
			networkId = foundId
			emailFallback = true
		}

	}

	feeFraction := 0.3
	netRevenue := model.UsdToNanoCents((1.0 - feeFraction) * float64(total) / 100.0)

	startTime := time.Unix(periodStart, 0)
	endTime := time.Unix(periodEnd, 0).Add(SubscriptionGracePeriod)

	if networkId == nil {
		return nil, fmt.Errorf("could not find network for invoice %s", invoiceId)
	}

	credited, err := stripeCreditInvoicePaid(
		clientSession.Ctx,
		*networkId,
		invoiceId,
		netRevenue,
		startTime,
		endTime,
	)
	if err != nil {
		glog.Infof("Error processing invoice paid: %v", err)
		return nil, err
	}

	// S11: every credit that resolved its network by the legacy email fallback
	// is an operator-visible audit row. Only on an actual credit -- a
	// redelivery of an already-credited invoice re-resolves the email but must
	// not inflate the count. The audit write must never fail the credit.
	if credited && emailFallback {
		if eventErr := model.AddPaymentReconciliationEvent(clientSession.Ctx, &model.PaymentReconciliationEvent{
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketStripe,
			NetworkId: networkId,
			Action:    model.PaymentReconcileActionEmailFallback,
			Evidence:  invoiceId,
			Details: map[string]any{
				"subscription":   subscriptionId,
				"resolution":     "customer_email",
				"customer_email": fullInvoice.Customer.Email,
			},
		}); eventErr != nil {
			glog.Errorf("[sub]invoice %s: could not record email_fallback event: %s\n", invoiceId, eventErr)
		}
	}

	return &StripeWebhookResult{}, nil

}

// stripeCreditInvoicePaid is the ONE place an invoice.paid turns into a credit,
// gated by the stripe_invoice ledger -- the same two-ledger shape the Apple path
// uses (apple_subscription_transaction). Stripe delivers at-least-once, and the
// 200 is only sent after commit, so a crash between commit and response
// GUARANTEES a redelivery: without this gate the renewal upsert absorbed the
// duplicate silently while AddTransferBalanceInTx inserted ANOTHER 600 GiB pro
// balance and double-counted SubsidyNetRevenue (which drives provider subsidy
// payouts).
//
// The ledger insert (ON CONFLICT DO NOTHING, rows-affected checked) and the
// credit are in the SAME tx: either both commit or neither does, and a
// concurrent duplicate delivery blocks on the ledger row and then sees zero
// rows affected. credited = false means this invoice was already credited.
func stripeCreditInvoicePaid(
	ctx context.Context,
	networkId server.Id,
	invoiceId string,
	netRevenue model.NanoCents,
	startTime time.Time,
	endTime time.Time,
) (credited bool, returnErr error) {

	server.Tx(ctx, func(tx server.PgTx) {

		ledgerTag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO stripe_invoice (invoice_id, network_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
			`,
			invoiceId,
			networkId,
		))
		if ledgerTag.RowsAffected() == 0 {
			// a retry of an invoice that already credited -- absorb it
			glog.Infof("[sub]invoice %s already credited; ignoring redelivery\n", invoiceId)
			return
		}

		subscriptionRenewal := model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          startTime,
			EndTime:            endTime,
			NetRevenue:         netRevenue,
			SubscriptionMarket: model.SubscriptionMarketStripe,
			TransactionId:      invoiceId,
		}

		err := model.AddSubscriptionRenewalInTx(tx, ctx, &subscriptionRenewal)
		if err != nil {
			glog.Infof("Error adding subscription renewal: %v", err)
			returnErr = err
			return
		}

		// a supporter subscription -> carries the Pro entitlement
		transferBalance := &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             startTime,
			EndTime:               endTime,
			StartBalanceByteCount: RefreshSupporterTransferBalance,
			SubsidyNetRevenue:     netRevenue,
			BalanceByteCount:      RefreshSupporterTransferBalance,
			Pro:                   true,
		}

		model.AddTransferBalanceInTx(
			ctx,
			tx,
			transferBalance,
		)

		credited = true
	})

	if returnErr != nil {
		return false, returnErr
	}

	if credited {
		// the pro balance is committed -- refresh the entitlement cache so the
		// upgrade is visible immediately rather than after ProCacheTtl
		model.UpdateProNetwork(ctx, networkId)
	}

	return credited, nil
}

// ----- refunds / disputes (UPGRADE.md §2 S7) -----
//
// decision table (charge -> what was granted -> clawback):
//
//	charge carries an invoice, invoice in the stripe_invoice ledger
//	    -> end the invoice's renewal + the pro balance it granted
//	       (EndReconciledEntitlementForTransactions, market=stripe scoped to
//	       that invoice -- the same shape the reconciler's end repair uses)
//	charge carries an invoice we never credited        -> refund_unmatched event
//	no invoice: charge -> checkout session -> the balance-code ledger
//	    (purchase_event_id = session id / session id per line)
//	    unredeemed code -> void it (cancel_time); redeemed -> end the granted
//	    transfer_balance
//	no invoice and no checkout session / no codes      -> refund_unmatched event
//
// A refund is delivered via BOTH charge.refunded and refund.created, so the
// clawback is gated on the refund id in the stripe_refund ledger, inside the
// same tx: one clawback and one operator event per refund, however many times
// and through whichever event types it arrives. Disputes are keyed by the
// dispute id with their own action label.

type stripeCheckoutSessionList struct {
	Data []*struct {
		Id string `json:"id"`
	} `json:"data"`
}

// stripeFetchCharge reads a charge back from the API -- refund.created and
// dispute events only carry the charge id, and the clawback needs the
// charge's invoice / payment_intent to find what it bought.
func stripeFetchCharge(ctx context.Context, chargeId string) (*StripeEventChargeObject, error) {
	url := fmt.Sprintf("%s/v1/charges/%s", stripeApiBaseUrl, chargeId)
	return server.HttpGetRequireStatusOk[*StripeEventChargeObject](
		ctx,
		url,
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
		},
		server.ResponseJsonObject[*StripeEventChargeObject],
	)
}

func stripeHandleChargeRefunded(
	charge *StripeEventChargeObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	refunds := []*StripeEventRefundObject{}
	if charge.Refunds != nil && 0 < len(charge.Refunds.Data) {
		refunds = charge.Refunds.Data
	} else {
		// newer API versions omit the embedded refund list from the event
		url := fmt.Sprintf("%s/v1/refunds?charge=%s&limit=100", stripeApiBaseUrl, charge.Id)
		refundList, err := server.HttpGetRequireStatusOk[*StripeEventChargeRefundList](
			clientSession.Ctx,
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
			},
			server.ResponseJsonObject[*StripeEventChargeRefundList],
		)
		if err != nil {
			// store API failure: non-2xx so Stripe redelivers
			return nil, fmt.Errorf("failed to list refunds for charge %s: %v", charge.Id, err)
		}
		refunds = refundList.Data
	}
	if len(refunds) == 0 {
		// a refunded charge with no listable refund objects: still claw back
		// exactly once, keyed on the charge itself
		refunds = []*StripeEventRefundObject{{
			Id:     "charge/" + charge.Id,
			Charge: charge.Id,
			Amount: charge.AmountRefunded,
		}}
	}

	for _, refund := range refunds {
		if err := stripeHandleRefund(
			clientSession,
			refund.Id,
			charge.Id,
			charge.Invoice,
			charge.PaymentIntent,
			model.PaymentReconcileActionRefunded,
			"charge.refunded",
			refund.Amount,
		); err != nil {
			return nil, err
		}
	}
	return &StripeWebhookResult{}, nil
}

func stripeHandleRefundCreated(
	refund *StripeEventRefundObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	if refund.Id == "" || refund.Charge == "" {
		glog.Infof("[sub]refund.created with no refund/charge id; ignoring\n")
		return &StripeWebhookResult{}, nil
	}
	charge, err := stripeFetchCharge(clientSession.Ctx, refund.Charge)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch charge %s: %v", refund.Charge, err)
	}
	if err := stripeHandleRefund(
		clientSession,
		refund.Id,
		charge.Id,
		charge.Invoice,
		charge.PaymentIntent,
		model.PaymentReconcileActionRefunded,
		"refund.created",
		refund.Amount,
	); err != nil {
		return nil, err
	}
	return &StripeWebhookResult{}, nil
}

func stripeHandleDisputeCreated(
	dispute *StripeEventDisputeObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	if dispute.Id == "" || dispute.Charge == "" {
		glog.Infof("[sub]charge.dispute.created with no dispute/charge id; ignoring\n")
		return &StripeWebhookResult{}, nil
	}
	charge, err := stripeFetchCharge(clientSession.Ctx, dispute.Charge)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch charge %s: %v", dispute.Charge, err)
	}
	if err := stripeHandleRefund(
		clientSession,
		dispute.Id,
		charge.Id,
		charge.Invoice,
		charge.PaymentIntent,
		model.PaymentReconcileActionDisputed,
		"charge.dispute.created",
		dispute.Amount,
	); err != nil {
		return nil, err
	}
	return &StripeWebhookResult{}, nil
}

// stripeHandleRefund is the ONE place a Stripe refund or dispute turns into a
// clawback, gated by the stripe_refund ledger keyed on refundId (the refund
// or dispute id). The ledger insert, the clawback, and the operator event
// commit in the SAME tx: either the refund is fully processed or Stripe
// retries it. Where the charge cannot be mapped to anything we granted, a
// refund_unmatched event is recorded for the operator instead of guessing --
// and the handler still answers 200, since a retry cannot do better.
func stripeHandleRefund(
	clientSession *session.ClientSession,
	refundId string,
	chargeId string,
	invoiceId string,
	paymentIntentId string,
	action string,
	eventType string,
	amount int,
) error {
	ctx := clientSession.Ctx
	now := server.NowUtc()

	// resolve what the charge bought BEFORE the clawback tx (store API reads)
	var ledgerNetworkId *server.Id
	stripeSessionId := ""
	if invoiceId != "" {
		if networkId, ok := model.GetStripeInvoiceNetworkId(ctx, invoiceId); ok {
			ledgerNetworkId = &networkId
		}
	} else if paymentIntentId != "" {
		// a single-charge (data pack) purchase: the checkout session id keys
		// the balance-code ledger
		url := fmt.Sprintf("%s/v1/checkout/sessions?payment_intent=%s", stripeApiBaseUrl, paymentIntentId)
		sessions, err := server.HttpGetRequireStatusOk[*stripeCheckoutSessionList](
			ctx,
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
			},
			server.ResponseJsonObject[*stripeCheckoutSessionList],
		)
		if err != nil {
			return fmt.Errorf("failed to fetch checkout session for payment intent %s: %v", paymentIntentId, err)
		}
		if 0 < len(sessions.Data) {
			stripeSessionId = sessions.Data[0].Id
		}
	}

	handled := false
	eventAction := action
	eventNetworkId := ledgerNetworkId
	details := map[string]any{
		"charge":     chargeId,
		"event_type": eventType,
	}
	if 0 < amount {
		details["amount"] = amount
	}
	endedNetworkIds := []server.Id{}

	server.Tx(ctx, func(tx server.PgTx) {
		ledgerTag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO stripe_refund (refund_id, charge_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
			`,
			refundId,
			chargeId,
		))
		if ledgerTag.RowsAffected() == 0 {
			// the same refund via the other event type (or a redelivery) --
			// exactly one clawback
			glog.Infof("[sub]refund %s already processed; ignoring redelivery\n", refundId)
			return
		}
		handled = true

		if invoiceId != "" {
			details["invoice"] = invoiceId
			if ledgerNetworkId != nil {
				// a subscription invoice we credited: end its renewal and the
				// pro balance it granted
				endedNetworkIds = model.EndReconciledEntitlementForTransactionsInTx(
					tx,
					ctx,
					model.SubscriptionMarketStripe,
					[]string{invoiceId},
					now,
				)
				details["ended"] = 0 < len(endedNetworkIds)
			} else {
				// the invoice never credited here -- nothing was granted,
				// nothing to claw back
				eventAction = model.PaymentReconcileActionRefundUnmatched
				details["reason"] = "invoice_not_credited"
			}
		} else if stripeSessionId != "" {
			details["checkout_session"] = stripeSessionId
			found, voidedCount, clawedNetworkIds := model.ClawbackBalanceCodesForPurchaseEventInTx(
				tx,
				ctx,
				stripeSessionId,
				now,
			)
			if found {
				endedNetworkIds = clawedNetworkIds
				details["voided_codes"] = voidedCount
				details["ended_balances"] = len(clawedNetworkIds)
			} else {
				eventAction = model.PaymentReconcileActionRefundUnmatched
				details["reason"] = "no_balance_code_for_session"
			}
		} else {
			// no invoice and no checkout session: this charge maps to nothing
			// we granted -- record it for the operator, never guess
			eventAction = model.PaymentReconcileActionRefundUnmatched
			details["reason"] = "charge_not_mapped"
		}

		if eventNetworkId == nil && 0 < len(endedNetworkIds) {
			eventNetworkId = &endedNetworkIds[0]
		}

		// stamp the outcome on the ledger row
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE stripe_refund SET network_id = $2, action = $3
			WHERE refund_id = $1
			`,
			refundId,
			eventNetworkId,
			eventAction,
		))

		if eventErr := model.AddPaymentReconciliationEventInTx(tx, ctx, &model.PaymentReconciliationEvent{
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketStripe,
			NetworkId: eventNetworkId,
			Action:    eventAction,
			Evidence:  refundId,
			Details:   details,
		}); eventErr != nil {
			// the event is part of the atomic unit: abort the tx (rolling back
			// the ledger row and the clawback) so Stripe retries the whole thing
			server.Raise(eventErr)
		}
	})

	if handled {
		for _, networkId := range endedNetworkIds {
			// the entitlement changed under the network -- make the downgrade
			// visible immediately rather than after ProCacheTtl
			model.UpdateProNetwork(ctx, networkId)
		}
		glog.Infof(
			"[sub]%s %s (charge %s): action=%s ended=%d\n",
			eventType, refundId, chargeId, eventAction, len(endedNetworkIds),
		)
	}
	return nil
}

type SubscriptionType string

const (
	SubscriptionTypeMonthly SubscriptionType = "monthly"
	SubscriptionTypeYearly  SubscriptionType = "yearly"
)

type StripeCreatePaymentIntentArgs struct{}

type StripeCreatePaymentIntentArgsErr struct {
	Message string `json:"message"`
}

type StripePaymentIntent struct {
	SubscriptionType SubscriptionType `json:"subscription_type"`
	ClientSecret     string           `json:"client_secret"`
}

type StripeCreatePaymentIntentResult struct {
	PaymentIntents []StripePaymentIntent             `json:"payment_intents,omitempty"`
	EphemeralKey   *string                           `json:"ephemeral_key,omitempty"`
	CustomerId     *string                           `json:"customer_id,omitempty"`
	PublishableKey *string                           `json:"publishable_key,omitempty"`
	Error          *StripeCreatePaymentIntentArgsErr `json:"error,omitempty"`
}

func StripeCreatePaymentIntent(
	args *StripeCreatePaymentIntentArgs,
	session *session.ClientSession,
) (*StripeCreatePaymentIntentResult, error) {

	// check if user has a stripe customer id
	stripeCustomerId, _ := model.GetStripeCustomer(session)

	stripe.Key = stripeApiToken()

	if stripeCustomerId == nil {
		// create a new stripe customer
		params := &stripe.CustomerParams{}
		result, err := customer.New(params)
		if err != nil {
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to create stripe customer: %v", err)},
			}, nil
		}

		err = model.CreateStripeCustomer(result.ID, session)
		if err != nil {
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to save stripe customer: %v", err)},
			}, nil
		}

		stripeCustomerId = &result.ID
	}

	ekparams := &stripe.EphemeralKeyParams{
		Customer:      stripeCustomerId,
		StripeVersion: stripe.String("2023-08-16"),
	}

	ek, err := ephemeralkey.New(ekparams)
	if err != nil {
		return &StripeCreatePaymentIntentResult{
			Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to create stripe ephemeral key: %v", err)},
		}, nil
	}

	// prices := stripeSubscriptionPrices()

	type SubPrice struct {
		PriceId string
		Type    SubscriptionType
	}

	var subPrices []SubPrice = []SubPrice{
		// stripeSubscriptionPrices().Monthly,
		// stripeSubscriptionPrices().Yearly,
		SubPrice{
			PriceId: "price_1RGDllEqqTaiwAGPOOvPrHUX",
			Type:    SubscriptionTypeMonthly,
		},
		SubPrice{
			PriceId: "price_1S5Bd2EqqTaiwAGPuCdDFm6a",
			Type:    SubscriptionTypeYearly,
		},
	}

	var intents []StripePaymentIntent

	for _, subPrice := range subPrices {

		params := &stripe.SubscriptionParams{
			Customer: stripeCustomerId,
			Items: []*stripe.SubscriptionItemsParams{
				{Price: &subPrice.PriceId},
			},
			PaymentBehavior: stripe.String("default_incomplete"),
			// Save payment method for future renewals
			PaymentSettings: &stripe.SubscriptionPaymentSettingsParams{
				SaveDefaultPaymentMethod: stripe.String("on_subscription"),
			},
			Metadata: map[string]string{
				"network_id": session.ByJwt.NetworkId.String(),
			},
			Expand: []*string{
				stripe.String("latest_invoice"),
			},
		}

		sub, err := subscription.New(params)
		if err != nil {
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to create stripe subscription: %v", err)},
			}, nil
		}

		if sub.LatestInvoice == nil || sub.LatestInvoice.ID == "" {
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: "No latest invoice found on the subscription"},
			}, nil
		}

		// Define helper structs to unmarshal the invoice with PaymentIntent
		type PaymentIntent struct {
			ID           string `json:"id"`
			ClientSecret string `json:"client_secret"`
		}

		type InvoiceWithPI struct {
			PaymentIntent *PaymentIntent `json:"payment_intent"`
		}

		// Fetch the invoice with PaymentIntent expanded
		url := fmt.Sprintf(
			stripeApiBaseUrl+"/v1/invoices/%s?expand[]=payment_intent",
			sub.LatestInvoice.ID,
		)

		invoice, err := server.HttpGetRequireStatusOk[*InvoiceWithPI](
			session.Ctx,
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
			},
			server.ResponseJsonObject[*InvoiceWithPI],
		)

		if err != nil {
			glog.Infof("Failed to fetch invoice details: %v", err)
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to fetch invoice details: %v", err)},
			}, nil
		}

		if invoice.PaymentIntent == nil || invoice.PaymentIntent.ClientSecret == "" {
			glog.Infof("No payment intent found on the latest invoice")
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: "No payment intent found on the latest invoice"},
			}, nil
		}

		intents = append(intents, StripePaymentIntent{
			ClientSecret:     invoice.PaymentIntent.ClientSecret,
			SubscriptionType: subPrice.Type,
		})
	}

	pk := stripePublishableKey()

	return &StripeCreatePaymentIntentResult{
		PaymentIntents: intents,
		EphemeralKey:   &ek.Secret,
		CustomerId:     stripeCustomerId,
		PublishableKey: &pk,
	}, nil
}

type StripeCreateCustomerPortalError struct {
	Message string `json:"message"`
}

type StripeCreateCustomerPortalResult struct {
	Url   string                           `json:"url,omitempty"`
	Error *StripeCreateCustomerPortalError `json:"error,omitempty"`
}

type StripeCreateCustomerPortalArgs struct{}

/**
 * Used to create a Stripe URL for the customer to manage their subscription
 */
func StripeCreateCustomerPortal(
	args *StripeCreateCustomerPortalArgs,
	session *session.ClientSession,
) (*StripeCreateCustomerPortalResult, error) {

	stripeCustomerId, err := model.GetStripeCustomer(session)
	if err != nil || stripeCustomerId == nil {
		return &StripeCreateCustomerPortalResult{
			Error: &StripeCreateCustomerPortalError{Message: "No stripe customer found"},
		}, nil
	}

	stripe.Key = stripeApiToken()

	params := &stripe.BillingPortalSessionParams{
		Customer: stripeCustomerId,
		// ReturnURL: stripe.String("https://example.com/account"),
	}

	result, err := stripesession.New(params)

	if err != nil {
		return &StripeCreateCustomerPortalResult{
			Error: &StripeCreateCustomerPortalError{Message: fmt.Sprintf("Failed to create stripe customer portal session: %v", err)},
		}, nil
	}

	return &StripeCreateCustomerPortalResult{
		Url: result.URL,
	}, nil
}

func UnsubscribeStripe(session *session.ClientSession) error {

	var subscriptionRenewals []struct {
		TransactionId string
		EndTime       time.Time
	}
	var queryErr error

	server.Tx(session.Ctx, func(tx server.PgTx) {

		// query if network has active stripe subscriptions
		result, err := tx.Query(
			session.Ctx,
			`
			SELECT transaction_id, end_time
			FROM subscription_renewal
			WHERE
				network_id = $1
				AND market = $2
				AND end_time > $3
			ORDER BY end_time DESC
			`,
			session.ByJwt.NetworkId,
			model.SubscriptionMarketStripe,
			server.NowUtc(),
		)

		if err != nil {
			glog.Errorf("[unsubscribe] Failed to query subscription_renewal: %v", err)
			queryErr = err
			return
		}

		server.WithPgResult(result, err, func() {
			for result.Next() {

				var txId string
				var endTime time.Time

				err := result.Scan(&txId, &endTime)
				if err != nil {
					glog.Errorf("[unsubscribe] Failed to scan subscription renewal: %v", err)
					queryErr = err
					continue
				}

				subscriptionRenewals = append(subscriptionRenewals, struct {
					TransactionId string
					EndTime       time.Time
				}{
					TransactionId: txId,
					EndTime:       endTime,
				})

			}
		})

	})

	if queryErr != nil {
		return fmt.Errorf("[unsubscribe] failed to query subscription renewals: %w", queryErr)
	}

	if len(subscriptionRenewals) == 0 {
		return nil
	}

	stripe.Key = stripeApiToken()

	for _, renewal := range subscriptionRenewals {

		invoiceId := renewal.TransactionId
		if invoiceId == "" {
			glog.Infof("[unsubscribe] Subscription renewal with empty transaction id, skipping")
			continue
		}

		// Get invoice details with expanded subscription info
		url := fmt.Sprintf("%s/v1/invoices/%s?expand[]=subscription&expand[]=customer", stripeApiBaseUrl, invoiceId)
		fullInvoice, err := server.HttpGetRequireStatusOk[*StripeInvoiceExpanded](
			session.Ctx,
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
			},
			server.ResponseJsonObject[*StripeInvoiceExpanded],
		)

		if err != nil {
			glog.Errorf("[unsubscribe] Failed to fetch invoice %s: %v", invoiceId, err)
			continue
		}

		// Extract subscription ID
		var subscriptionId string
		if fullInvoice.Subscription != nil && fullInvoice.Subscription.ID != "" {
			subscriptionId = fullInvoice.Subscription.ID
		}

		if subscriptionId == "" {
			glog.Errorf("[unsubscribe] No subscription ID found for invoice %s", invoiceId)
			continue
		}

		// Cancel the subscription
		glog.Infof("[unsubscribe] Canceling Stripe subscription %s for network %s", subscriptionId, session.ByJwt.NetworkId)

		cancelUrl := fmt.Sprintf("%s/v1/subscriptions/%s", stripeApiBaseUrl, subscriptionId)

		req, err := http.NewRequestWithContext(session.Ctx, "DELETE", cancelUrl, nil)
		if err != nil {
			glog.Errorf("[unsubscribe] Failed to create cancel request for subscription %s: %v", subscriptionId, err)
			continue
		}

		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))

		httpClient := server.DefaultHttpClient()
		resp, err := httpClient.Do(req)
		if err != nil {
			glog.Errorf("[unsubscribe] Failed to cancel Stripe subscription %s: %v", subscriptionId, err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			glog.Errorf("[unsubscribe] Stripe API returned error status %d for subscription %s", resp.StatusCode, subscriptionId)
			continue
		}

		glog.Infof("[unsubscribe] Successfully canceled Stripe subscription %s", subscriptionId)

	}

	// set subscription_renewal end time as now for all active subscriptions
	// to prevent transfer balance from being unnecessarily added
	server.Tx(session.Ctx, func(tx server.PgTx) {

		_, err := tx.Exec(
			session.Ctx,
			`
			UPDATE subscription_renewal
			SET end_time = $1
			WHERE
				network_id = $2
				AND market = $3
				AND end_time > $4
			`,
			server.NowUtc(),
			session.ByJwt.NetworkId,
			model.SubscriptionMarketStripe,
			server.NowUtc(),
		)

		if err != nil {
			glog.Errorf("[unsubscribe] Failed to update subscription_renewal end times: %v", err)
		}

	})

	return nil
}

// ----- Checkout Session -----
//
// This is what the web upgrade/buy-data flows were missing entirely. The only payment
// primitive the API offered was /subscription/create-payment-id, which mints a
// correlation token -- NOT a Stripe session -- so the web client had nowhere to send
// the user and its "Subscribe with Stripe" button could only ever throw.

// The things a customer can buy on the web.
//
// Pro maps to the Stripe subscription PRICES (config stripe.yml subscription_prices).
// The data packs map to Stripe PRODUCTS (config stripe.yml skus) with the amount
// attached from pro.yml -- so price lives in one place (pro.yml) and the webhook can
// still match the product back to a sku to know how much data to grant.
const (
	StripeItemProMonthly = "pro_monthly"
	StripeItemProYearly  = "pro_yearly"
	StripeItemData1Tib   = "data_1tib"
	StripeItemData10Tib  = "data_10tib"
)

// How the customer will be shown Checkout.
//
// These are Stripe's own `ui_mode` values, and they are MUTUALLY EXCLUSIVE on a single
// session -- this is a Stripe constraint, not ours:
//
//   - hosted   -> the session has a `url` and NO client_secret. success_url/cancel_url
//     apply. The client navigates away to checkout.stripe.com.
//   - embedded -> the session has a `client_secret` and NO `url`. success_url/cancel_url
//     are REJECTED by the API; return_url applies instead. Stripe.js mounts
//     Checkout in an iframe on our own page.
//
// So one session cannot hand back both a url and a client secret, and the caller has to
// say which one it wants. Desktop (Windows/Linux) asks for embedded and renders it in a
// webview; anything that can only open a browser asks for hosted.
const (
	StripeUiModeHosted   = "hosted"
	StripeUiModeEmbedded = "embedded"
)

type StripeCreateCheckoutSessionArgs struct {
	ItemId string `json:"item_id"`
	// "hosted" (default) or "embedded". Defaults to hosted so existing callers, which
	// only ever read checkout_url, keep working unchanged.
	UiMode string `json:"ui_mode,omitempty"`
	// "never" keeps an EMBEDDED checkout fully inline: Stripe fires the client's
	// onComplete callback instead of redirecting anywhere, so the page the customer is
	// on (e.g. the account panel) never navigates. Only valid with ui_mode "embedded".
	// Empty means the embedded flow redirects to the configured return_url, and hosted
	// behaves as always.
	RedirectOnCompletion string `json:"redirect_on_completion,omitempty"`
}

type StripeCreateCheckoutSessionError struct {
	Message string `json:"message"`
}

type StripeCreateCheckoutSessionResult struct {
	// Which of the two shapes below is populated.
	UiMode string `json:"ui_mode,omitempty"`

	// HOSTED: Stripe's hosted checkout url. The client simply navigates here -- no
	// Stripe.js and no publishable key needed.
	CheckoutUrl string `json:"checkout_url,omitempty"`

	// EMBEDDED: the client secret Stripe.js needs to mount Embedded Checkout, plus the
	// publishable key to initialize Stripe.js with. The publishable key is not a secret
	// (StripeCreatePaymentIntent already hands it to mobile the same way).
	ClientSecret   string `json:"client_secret,omitempty"`
	PublishableKey string `json:"publishable_key,omitempty"`

	// Both modes. The caller can use this to reconcile the purchase afterwards.
	SessionId string `json:"session_id,omitempty"`

	Error *StripeCreateCheckoutSessionError `json:"error,omitempty"`
}

func stripeCheckoutError(message string) *StripeCreateCheckoutSessionResult {
	return &StripeCreateCheckoutSessionResult{
		Error: &StripeCreateCheckoutSessionError{Message: message},
	}
}

// stripeDataPackLineItems builds the one line item of a data pack checkout: priced
// from pro.yml and attached to the EXISTING Stripe product -- so
// checkout.session.completed can still look the sku up by product id and know how
// much data to grant. Shared by the signed-in checkout and /pay/data/checkout so
// both sell exactly the same thing. A non-empty errMessage is customer-facing.
func stripeDataPackLineItems(itemId string) (lineItems []*stripe.CheckoutSessionLineItemParams, errMessage string) {
	byteCount, ok := stripeDataPackByteCount(itemId)
	if !ok {
		return nil, "Unknown item."
	}
	productId, ok := stripeProductForByteCount(byteCount)
	if !ok {
		glog.Errorf("[stripe]no product configured for data pack %s\n", itemId)
		return nil, "That data pack is not available."
	}
	priceUsd, ok := stripeDataPackPriceUsd(byteCount)
	if !ok || priceUsd <= 0 {
		glog.Errorf("[stripe]no price in pro.yml for data pack %s\n", itemId)
		return nil, "That data pack is not available."
	}
	return []*stripe.CheckoutSessionLineItemParams{
		{
			PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
				Currency:   stripe.String(string(stripe.CurrencyUSD)),
				Product:    stripe.String(productId),
				UnitAmount: stripe.Int64(int64(math.Round(priceUsd * 100))),
			},
			Quantity: stripe.Int64(1),
		},
	}, ""
}

// stripeDataPackByteCount maps a data item id to the amount it grants, from pro.yml.
func stripeDataPackByteCount(itemId string) (model.ByteCount, bool) {
	switch itemId {
	case StripeItemData1Tib:
		return 1 * model.Tib, true
	case StripeItemData10Tib:
		return 10 * model.Tib, true
	}
	return 0, false
}

// stripeProductForByteCount finds the Stripe product configured for a data amount, so
// the fulfilment webhook (which looks a sku up by product id) can tell how much data
// was bought. The supporter sku is not a data pack.
func stripeProductForByteCount(byteCount model.ByteCount) (string, bool) {
	for productId, sku := range stripeSkus() {
		if sku.Supporter {
			continue
		}
		if sku.BalanceByteCount() == byteCount {
			return productId, true
		}
	}
	return "", false
}

// stripeDataPackPriceUsd is the price of a data amount, from pro.yml -- the same source
// the site and the x402 skus quote from, so a customer is never shown two prices.
func stripeDataPackPriceUsd(byteCount model.ByteCount) (float64, bool) {
	for _, sku := range model.Pro().DataCodeSkus {
		if sku.Data == byteCount {
			return sku.PriceUsd, true
		}
	}
	return 0, false
}

// stripeCheckoutUiMode normalizes the requested ui mode. Empty means hosted, so callers
// written before embedded existed keep getting exactly what they got before.
func stripeCheckoutUiMode(uiMode string) (string, bool) {
	switch uiMode {
	case "":
		return StripeUiModeHosted, true
	case StripeUiModeHosted, StripeUiModeEmbedded:
		return uiMode, true
	}
	return "", false
}

// stripeCheckoutRedirectNever validates redirect_on_completion against the ui mode.
// "never" is an EMBEDDED-only concept: hosted checkout lives on stripe.com and MUST
// come back somewhere.
func stripeCheckoutRedirectNever(uiMode string, redirectOnCompletion string) (bool, bool) {
	switch redirectOnCompletion {
	case "":
		return false, true
	case "never":
		return uiMode == StripeUiModeEmbedded, uiMode == StripeUiModeEmbedded
	}
	return false, false
}

// stripeCheckoutApplyUiMode sets the ui-mode-specific params, and reports whether this
// env is configured for that mode at all.
//
// The two shapes MUST NOT be mixed: Stripe rejects a session that carries
// success_url/cancel_url with ui_mode embedded, and an embedded session with no
// return_url has nowhere to send the customer when they finish. Keeping this in one
// place (rather than inline) is what makes it testable without a Stripe key.
func stripeCheckoutApplyUiMode(
	params *stripe.CheckoutSessionParams,
	uiMode string,
	redirectNever bool,
	urls StripeCheckoutUrls,
) bool {
	switch uiMode {
	case StripeUiModeEmbedded:
		params.UIMode = stripe.String(string(stripe.CheckoutSessionUIModeEmbedded))
		if redirectNever {
			// fully inline: Stripe fires onComplete in the client and never navigates.
			// Stripe rejects a session carrying BOTH return_url and "never", so the
			// return url is omitted -- which also means this works in an env that has
			// no checkout.return_url configured.
			params.RedirectOnCompletion = stripe.String("never")
			return true
		}
		if urls.ReturnUrl == "" {
			return false
		}
		params.ReturnURL = stripe.String(urls.ReturnUrl)
		return true

	case StripeUiModeHosted:
		if urls.SuccessUrl == "" || urls.CancelUrl == "" {
			return false
		}
		params.SuccessURL = stripe.String(urls.SuccessUrl)
		params.CancelURL = stripe.String(urls.CancelUrl)
		return true
	}
	return false
}

// StripeCreateCheckoutSession creates a real Stripe Checkout Session, either hosted
// (returns Stripe's url) or embedded (returns a client secret for Stripe.js). See the
// StripeUiMode* consts: Stripe gives a session ONE of the two, never both.
//
// The session carries client_reference_id = networkId. That is how BOTH webhooks find
// the network to fulfil (stripeHandleInvoicePaid for Pro, and the checkout-complete
// handler for data packs). Removing it would take the money and deliver nothing.
// StripeSubscriptionTrialDays is the free trial the YEARLY Stripe Pro
// subscription starts with, in days; the monthly plan has no trial. It is the
// ONE number for the trial across the products sold through this endpoint; the
// offers configured elsewhere must match it: the Android app's FREE_TRIAL_DAYS
// and the yearly Stripe payment link it opens, the Play Console yearly offer,
// and the App Store yearly introductory offer (which only allows fixed
// durations, so iOS shows the nearest one StoreKit reports).
const StripeSubscriptionTrialDays = 15

func StripeCreateCheckoutSession(
	args *StripeCreateCheckoutSessionArgs,
	clientSession *session.ClientSession,
) (*StripeCreateCheckoutSessionResult, error) {

	uiMode, ok := stripeCheckoutUiMode(args.UiMode)
	if !ok {
		return stripeCheckoutError("Unknown ui mode."), nil
	}

	redirectNever, redirectOk := stripeCheckoutRedirectNever(uiMode, args.RedirectOnCompletion)
	if !redirectOk {
		return &StripeCreateCheckoutSessionResult{
			Error: &StripeCreateCheckoutSessionError{
				Message: "redirect_on_completion \"never\" requires ui_mode \"embedded\".",
			},
		}, nil
	}

	networkId := clientSession.ByJwt.NetworkId

	params := &stripe.CheckoutSessionParams{
		// the fulfilment webhooks resolve the network from this
		ClientReferenceID: stripe.String(networkId.String()),
	}

	// Where the customer lands when they are done. Note this runs BEFORE any vault access
	// below, so an env with no stripe config refuses cleanly instead of panicking on a
	// missing secret.
	if !stripeCheckoutApplyUiMode(params, uiMode, redirectNever, stripeCheckoutUrls()) {
		// refuse rather than hand a customer to Stripe with no way back
		glog.Errorf("[stripe]checkout urls are not configured for ui mode %s\n", uiMode)
		return stripeCheckoutError("Checkout is not configured."), nil
	}

	stripe.Key = stripeApiToken()

	switch args.ItemId {

	case StripeItemProMonthly, StripeItemProYearly:
		prices := stripeSubscriptionPrices()
		priceId := prices.Monthly
		if args.ItemId == StripeItemProYearly {
			priceId = prices.Yearly
		}
		if priceId == "" {
			glog.Errorf("[stripe]no subscription price configured for %s\n", args.ItemId)
			return stripeCheckoutError("That plan is not available."), nil
		}

		params.Mode = stripe.String(string(stripe.CheckoutSessionModeSubscription))
		params.LineItems = []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceId),
				Quantity: stripe.Int64(1),
			},
		}
		// Stamp the network onto the SUBSCRIPTION itself, not just the session.
		// stripeHandleInvoicePaid resolves the network in this order:
		//   1. subscription metadata network_id  <- this
		//   2. the Stripe customer's email -> FindNetworkIdByEmail
		//   3. the checkout session's client_reference_id
		// Without (1) a customer who pays with a different email than their account
		// falls through to (2), which can resolve to the WRONG network or none at all.
		// Renewal invoices also carry the subscription but no checkout session, so (3)
		// is not a reliable long-term anchor either.
		params.SubscriptionData = &stripe.CheckoutSessionSubscriptionDataParams{
			Metadata: map[string]string{
				"network_id": networkId.String(),
			},
		}
		// only the yearly plan starts with the free trial (the web app, windows
		// and linux sell Pro through these sessions)
		if args.ItemId == StripeItemProYearly {
			params.SubscriptionData.TrialPeriodDays = stripe.Int64(StripeSubscriptionTrialDays)
		}

	case StripeItemData1Tib, StripeItemData10Tib:
		lineItems, errMessage := stripeDataPackLineItems(args.ItemId)
		if errMessage != "" {
			return stripeCheckoutError(errMessage), nil
		}
		// a one-time payment
		params.Mode = stripe.String(string(stripe.CheckoutSessionModePayment))
		params.LineItems = lineItems

	default:
		return stripeCheckoutError("Unknown item."), nil
	}

	checkoutSession, err := stripecheckout.New(params)
	if err != nil {
		glog.Errorf("[stripe]could not create checkout session for %s: %s\n", args.ItemId, err)
		return stripeCheckoutError("Could not start checkout. Please try again."), nil
	}

	glog.Infof(
		"[stripe]checkout session %s created for network %s item %s (%s)\n",
		checkoutSession.ID, networkId, args.ItemId, uiMode,
	)

	result := &StripeCreateCheckoutSessionResult{
		UiMode:    uiMode,
		SessionId: checkoutSession.ID,
	}
	switch uiMode {
	case StripeUiModeEmbedded:
		// Stripe.js needs BOTH of these to mount Embedded Checkout. There is no url on
		// an embedded session -- see the StripeUiMode* comment.
		result.ClientSecret = checkoutSession.ClientSecret
		result.PublishableKey = stripePublishableKey()
	case StripeUiModeHosted:
		result.CheckoutUrl = checkoutSession.URL
	}
	return result, nil
}
