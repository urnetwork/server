package controller

// Auto-renew's Stripe (DESIGN-COMPUTE §9.5, §9.8, CF-5; BAL-M12-11): sidecar/controller's ComputeStripeRenew on
// ComputeServerStripe, its payer check, and the renewals' events of compute's Stripe webhook branch.
//
//	CardFingerprint     a saved card's fingerprint, which the payer's keyed hash is made from
//	ChargeOffSession    threshold's top-up: a PaymentIntent for pack P on the card its payer saved, confirm=true and
//	                    off_session=true, under the job's idempotency key of network, UTC day, attempt and pack, card
//	                    payments only; its metadata names the network, the payer, the pack and compute_renewal=threshold,
//	                    never content. A card error is an answer, authentication_required or failed; Stripe not
//	                    answering is an error, and the job sends the same key again
//	MonthlyCheckout     the monthly plan: a Checkout session in subscription mode on the customer of its network and
//	                    member, one line of M credits a month at a dollar a credit on the compute product, tax added by
//	                    Stripe Tax, the subscription's metadata naming the network, the payer, M and
//	                    compute_renewal=monthly, so each of its invoices names them
//	CancelSubscription  the plan's subscription cancelled at once, so no invoice follows; one gone is no error
//	DetachCard          a saved card detached from its customer; one detached already is no error
//
// The webhook's renewals (renewWebhook, which compute's branch asks first):
//
//	payment_intent.succeeded of a top-up   its lot credited once and counted among threshold's day and month
//	                                       (sidecar/controller's ComputeTopUpCredit), whichever of it and the job comes
//	                                       first; a closed account's refunded
//	payment_intent.* else of a top-up      acknowledged: the job records a top-up's failures from its own answer
//	checkout.session.completed of a plan   the subscription recorded, monthly on (ComputeMonthlySubscribed)
//	invoice.paid of a plan                 the month's kind-2 lot, its market_ref the invoice's PaymentIntent
//	                                       (ComputeMonthlyGrant); a closed account's refunded and its plan cancelled
//	invoice.payment_failed of a plan       the billing contacts told (ComputeMonthlyPaymentFailed); Stripe's billing
//	                                       retry follows
//	invoice.voided, .marked_uncollectible  acknowledged, so Pro's dunning never reads a compute invoice
//	customer.subscription.deleted          monthly off (ComputeMonthlyEnded)
//
// A plan's invoice names compute by its subscription's metadata, which Stripe snapshots onto it
// (parent.subscription_details), never by amount; Pro's subscriptions carry none of compute's, so neither reaches the
// other's handler. Tax on a top-up, which no Checkout carries, is not computed in this draft: Stripe Tax's calculation
// API is not called (the report says so).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/glog"
	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The metadata of compute's renewals: identifiers.
const (
	// On a top-up's PaymentIntent and a plan's session and subscription: which renewal it is, threshold or monthly.
	// Never a pack checkout's compute_auto_renew, which names the mode a purchase turned on.
	computeStripeMetadataRenewal = "compute_renewal"
	// The plan's M, in credits, on its session and subscription.
	computeStripeMetadataMonthly = "compute_monthly_credits"
	// Stripe's words: a subscription's monthly interval, and a session's subscription mode.
	computeStripeIntervalMonth    = "month"
	computeStripeSessionModeSubsc = "subscription"
)

var _ sidecarcontroller.ComputeStripeRenew = (*ComputeServerStripe)(nil)

// See sidecarcontroller.ComputeStripeRenew: the card's fingerprint, "" for a payment method that is no card.
func (self *ComputeServerStripe) CardFingerprint(ctx context.Context, paymentMethodId string) (string, error) {
	client, err := self.client()
	if err != nil {
		return "", err
	}
	method, err := client.V1PaymentMethods.Retrieve(ctx, paymentMethodId, nil)
	if err != nil {
		return "", fmt.Errorf("compute stripe: the card %s: %w", paymentMethodId, err)
	}
	if method.Card == nil {
		return "", nil
	}
	return method.Card.Fingerprint, nil
}

// The metadata of a top-up's PaymentIntent: the network, the payer, the pack and the renewal, never content.
func computeStripeTopUpMetadata(charge *sidecarcontroller.ComputeOffSessionCharge) map[string]string {
	return map[string]string{
		ComputeStripeMetadataCompute: computeStripeMetadataVersion,
		computeStripeMetadataNetwork: charge.NetworkId.String(),
		computeStripeMetadataMember:  charge.PayerMemberId.String(),
		computeStripeMetadataPack:    strconv.FormatInt(charge.PackCredits, 10),
		computeStripeMetadataRenewal: computeStripeRenewThreshold,
	}
}

// See sidecarcontroller.ComputeStripeRenew (and the file header).
func (self *ComputeServerStripe) ChargeOffSession(ctx context.Context, charge *sidecarcontroller.ComputeOffSessionCharge) (*sidecarcontroller.ComputeOffSessionAnswer, error) {
	if charge.PackCredits <= 0 || charge.AmountNanoUsd != charge.PackCredits*sidecarmodel.ComputeCreditNanoUsd || charge.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: a top-up is a pack at a dollar a credit, under a key", sidecarcontroller.ErrComputeAutoRenewArgs)
	}
	client, err := self.client()
	if err != nil {
		return nil, err
	}
	params := &stripe.PaymentIntentCreateParams{
		Amount:             stripe.Int64(charge.PackCredits * computeStripeCentsPerCredit),
		Currency:           stripe.String(computeStripeCurrency),
		Customer:           stripe.String(charge.StripeCustomerId),
		PaymentMethod:      stripe.String(charge.StripePaymentMethodId),
		PaymentMethodTypes: []*string{stripe.String(computeStripePaymentCard)},
		Confirm:            stripe.Bool(true),
		OffSession:         stripe.Bool(true),
		Metadata:           computeStripeTopUpMetadata(charge),
	}
	params.SetIdempotencyKey(charge.IdempotencyKey)
	intent, err := client.V1PaymentIntents.Create(ctx, params)
	if err != nil {
		var stripeErr *stripe.Error
		if !errors.As(err, &stripeErr) {
			return nil, fmt.Errorf("compute stripe: a top-up: %w", err)
		}
		answer := &sidecarcontroller.ComputeOffSessionAnswer{Outcome: sidecarcontroller.ComputeOffSessionFailed}
		if stripeErr.PaymentIntent != nil {
			answer.PaymentIntentId = stripeErr.PaymentIntent.ID
		}
		switch {
		case stripeErr.Code == stripe.ErrorCodeAuthenticationRequired:
			answer.Outcome = sidecarcontroller.ComputeOffSessionAuthenticationRequired
			return answer, nil
		case stripeErr.Type == stripe.ErrorTypeCard:
			return answer, nil
		}
		return nil, fmt.Errorf("compute stripe: a top-up: %w", err)
	}
	answer := &sidecarcontroller.ComputeOffSessionAnswer{PaymentIntentId: intent.ID}
	switch intent.Status {
	case stripe.PaymentIntentStatusSucceeded:
		answer.Outcome = sidecarcontroller.ComputeOffSessionSucceeded
	case stripe.PaymentIntentStatusProcessing:
		answer.Outcome = sidecarcontroller.ComputeOffSessionProcessing
	case stripe.PaymentIntentStatusRequiresAction:
		answer.Outcome = sidecarcontroller.ComputeOffSessionAuthenticationRequired
	default:
		answer.Outcome = sidecarcontroller.ComputeOffSessionFailed
	}
	return answer, nil
}

// The metadata of a plan's session and subscription.
func computeStripeMonthlyMetadata(request *sidecarcontroller.ComputeStripeMonthlyRequest) map[string]string {
	return map[string]string{
		ComputeStripeMetadataCompute: computeStripeMetadataVersion,
		computeStripeMetadataNetwork: request.NetworkId.String(),
		computeStripeMetadataMember:  request.MemberId.String(),
		computeStripeMetadataMonthly: strconv.FormatInt(request.Credits, 10),
		computeStripeMetadataRenewal: computeStripeRenewMonthly,
	}
}

// See sidecarcontroller.ComputeStripeRenew (and the file header).
func (self *ComputeServerStripe) MonthlyCheckout(ctx context.Context, request *sidecarcontroller.ComputeStripeMonthlyRequest) (*sidecarcontroller.ComputeStripeCheckoutSession, error) {
	if request.Credits <= 0 || request.AmountNanoUsd != request.Credits*sidecarmodel.ComputeCreditNanoUsd {
		return nil, fmt.Errorf("%w: a plan is priced at a dollar a credit", sidecarcontroller.ErrComputeCheckoutArgs)
	}
	config, err := self.deps.Config()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", sidecarcontroller.ErrComputeStripeUnavailable, err)
	}
	client, err := self.client()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", sidecarcontroller.ErrComputeStripeUnavailable, err)
	}
	customerId, err := self.customer(ctx, client, request.NetworkId, request.MemberId)
	if err != nil {
		return nil, err
	}
	metadata := computeStripeMonthlyMetadata(request)
	subscriptionMetadata := map[string]string{}
	for key, value := range metadata {
		subscriptionMetadata[key] = value
	}
	params := &stripe.CheckoutSessionCreateParams{
		Mode:     stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer: stripe.String(customerId),
		CustomerUpdate: &stripe.CheckoutSessionCreateCustomerUpdateParams{
			Address: stripe.String(computeStripeAddressAuto),
		},
		AutomaticTax: &stripe.CheckoutSessionCreateAutomaticTaxParams{Enabled: stripe.Bool(true)},
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:    stripe.String(computeStripeCurrency),
				Product:     stripe.String(config.ProductId),
				UnitAmount:  stripe.Int64(request.Credits * computeStripeCentsPerCredit),
				TaxBehavior: stripe.String(computeStripeTaxExclusive),
				Recurring: &stripe.CheckoutSessionCreateLineItemPriceDataRecurringParams{
					Interval: stripe.String(computeStripeIntervalMonth),
				},
			},
			Quantity: stripe.Int64(1),
		}},
		PaymentMethodTypes: []*string{stripe.String(computeStripePaymentCard)},
		SubscriptionData:   &stripe.CheckoutSessionCreateSubscriptionDataParams{Metadata: subscriptionMetadata},
		Metadata:           metadata,
		SuccessURL:         stripe.String(config.SuccessUrl),
		CancelURL:          stripe.String(config.CancelUrl),
		ExpiresAt:          stripe.Int64(server.NowUtc().Add(self.settings.CheckoutExpireTime).Unix()),
	}
	session, err := client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("compute stripe: the plan's checkout session: %w", err)
	}
	if session.URL == "" || session.ExpiresAt <= 0 {
		return nil, errors.New("compute stripe: a plan's checkout session with no url or expiry")
	}
	return &sidecarcontroller.ComputeStripeCheckoutSession{
		SessionId:  session.ID,
		Url:        session.URL,
		ExpireTime: time.Unix(session.ExpiresAt, 0).UTC(),
	}, nil
}

// Whether err is Stripe's answer that the object is gone or already in the state asked for.
func computeStripeGone(err error) bool {
	var stripeErr *stripe.Error
	return errors.As(err, &stripeErr) && (stripeErr.Code == stripe.ErrorCodeResourceMissing || stripeErr.HTTPStatusCode == 404)
}

// See sidecarcontroller.ComputeStripeRenew (and the file header).
func (self *ComputeServerStripe) CancelSubscription(ctx context.Context, subscriptionId string) error {
	client, err := self.client()
	if err != nil {
		return err
	}
	subscription, err := client.V1Subscriptions.Retrieve(ctx, subscriptionId, nil)
	if computeStripeGone(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("compute stripe: the subscription %s: %w", subscriptionId, err)
	}
	if subscription.Status == stripe.SubscriptionStatusCanceled || subscription.Status == stripe.SubscriptionStatusIncompleteExpired {
		return nil
	}
	if _, err := client.V1Subscriptions.Cancel(ctx, subscriptionId, nil); err != nil && !computeStripeGone(err) {
		return fmt.Errorf("compute stripe: the cancel of %s: %w", subscriptionId, err)
	}
	return nil
}

// See sidecarcontroller.ComputeStripeRenew (and the file header).
func (self *ComputeServerStripe) DetachCard(ctx context.Context, paymentMethodId string) error {
	client, err := self.client()
	if err != nil {
		return err
	}
	method, err := client.V1PaymentMethods.Retrieve(ctx, paymentMethodId, nil)
	if computeStripeGone(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("compute stripe: the card %s: %w", paymentMethodId, err)
	}
	if method.Customer == nil || method.Customer.ID == "" {
		return nil
	}
	if _, err := client.V1PaymentMethods.Detach(ctx, paymentMethodId, &stripe.PaymentMethodDetachParams{}); err != nil && !computeStripeGone(err) {
		return fmt.Errorf("compute stripe: the detach of %s: %w", paymentMethodId, err)
	}
	return nil
}

// Whether a payer may still buy, as server's roles hold them (§9.10): a Team's Owner or Billing admin, as the frame
// lets buy until CACL's compute:buy grant lands; in a personal network its one person, the Owner, whose member the
// network's own devices name. A role that does not read is an error, and the job charges nothing that run.
type ComputeServerPayers struct{}

var _ sidecarcontroller.ComputeAutoRenewPayers = ComputeServerPayers{}

// See sidecarcontroller.ComputeAutoRenewPayers.
func (self ComputeServerPayers) MayBuy(env operator.Env, ctx context.Context, networkId operator.Id, memberId operator.Id) (bool, error) {
	mayBuy := false
	var readErr error
	server.HandleError(func() {
		if !server.SidecarMg6Linked() || !model.NetworkIsTeam(ctx, networkId) {
			mayBuy = true
			return
		}
		role, ok := model.MemberRole(ctx, networkId, memberId)
		mayBuy = ok && (role == model.NetworkMemberRoleOwner || role == model.NetworkMemberRoleBillingAdmin)
	}, func(err error) {
		readErr = err
	})
	if readErr != nil {
		return false, fmt.Errorf("compute stripe: the payer's role did not read: %w", readErr)
	}
	return mayBuy, nil
}

var installComputeAutoRenewOnce sync.Once

// Registers the process's Stripe for auto-renew and its payer check with sidecar/controller, once a process, with the
// ledger's keys the job hashes payers with: for the api's compute routes and webhook, and taskworker's
// compute_auto_renew. A second call does nothing.
func InstallComputeAutoRenew() {
	installComputeAutoRenewOnce.Do(func() {
		InstallComputeLedgerSecrets()
		sidecarcontroller.RegisterComputeStripeRenew(computeStripeProcess())
		sidecarcontroller.RegisterComputeAutoRenewPayers(ComputeServerPayers{})
	})
}

// A plan's or a top-up's metadata as the webhook reads it: the network, the payer and M or P.
type computeStripeRenewal struct {
	networkId server.Id
	memberId  server.Id
	credits   int64
}

// The renewal metadata names, refused where it is not compute's whole format, names another renewal, or credits the
// settings do not sell: a top-up's pack is a positive count up to the largest pack, a plan's one of the plans.
func computeStripeRenewalOf(ledger *sidecarmodel.ComputeLedgerSettings, metadata map[string]string, renewal string) (*computeStripeRenewal, error) {
	if !computeStripeMarked(metadata) || metadata[computeStripeMetadataRenewal] != renewal {
		return nil, errors.New("compute stripe: not this renewal")
	}
	networkId, err := server.ParseId(metadata[computeStripeMetadataNetwork])
	if err != nil {
		return nil, fmt.Errorf("compute stripe: the renewal's network: %w", err)
	}
	memberId, err := server.ParseId(metadata[computeStripeMetadataMember])
	if err != nil {
		return nil, fmt.Errorf("compute stripe: the renewal's member: %w", err)
	}
	parsed := &computeStripeRenewal{networkId: networkId, memberId: memberId}
	switch renewal {
	case computeStripeRenewThreshold:
		parsed.credits, err = strconv.ParseInt(metadata[computeStripeMetadataPack], 10, 64)
		if err != nil || parsed.credits <= 0 || len(ledger.StripePackCredits) == 0 || slices.Max(ledger.StripePackCredits) < parsed.credits {
			return nil, fmt.Errorf("compute stripe: a top-up's pack")
		}
	case computeStripeRenewMonthly:
		parsed.credits, err = strconv.ParseInt(metadata[computeStripeMetadataMonthly], 10, 64)
		if err != nil || !slices.Contains(ledger.MonthlyPlanCredits, parsed.credits) {
			return nil, fmt.Errorf("compute stripe: a plan's credits")
		}
	}
	return parsed, nil
}

// A plan's invoice as the webhook reads it.
type computeStripeInvoiceObject struct {
	Id       string `json:"id"`
	Subtotal int64  `json:"subtotal"`
	Currency string `json:"currency"`
	Parent   *struct {
		SubscriptionDetails *struct {
			Metadata     map[string]string `json:"metadata"`
			Subscription json.RawMessage   `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
}

// The renewals' events (see the file header), which compute's branch asks before its own: handled false where the
// event is no renewal of compute's.
func (self *ComputeServerStripe) renewWebhook(ctx context.Context, event *StripeWebhookArgs, object *computeStripeEventObject) (bool, error) {
	switch event.Type {
	case "payment_intent.succeeded", "payment_intent.amount_capturable_updated", "payment_intent.canceled", "payment_intent.payment_failed":
		if !computeStripeMarked(object.Metadata) || object.Metadata[computeStripeMetadataRenewal] == "" {
			return false, nil
		}
		if event.Type == "payment_intent.succeeded" {
			return true, self.topUpSucceeded(ctx, object.Id)
		}
		return true, nil
	case "checkout.session.completed":
		if !computeStripeMarked(object.Metadata) || object.Metadata[computeStripeMetadataRenewal] != computeStripeRenewMonthly {
			return false, nil
		}
		return true, self.monthlySubscribed(ctx, event.Data.Object)
	case "invoice.paid", "invoice.payment_failed", "invoice.voided", "invoice.marked_uncollectible":
		invoice := &computeStripeInvoiceObject{}
		if err := json.Unmarshal(event.Data.Object, invoice); err != nil || invoice.Parent == nil || invoice.Parent.SubscriptionDetails == nil {
			return false, nil
		}
		details := invoice.Parent.SubscriptionDetails
		if !computeStripeMarked(details.Metadata) || details.Metadata[computeStripeMetadataRenewal] != computeStripeRenewMonthly {
			return false, nil
		}
		if !self.deps.Active() {
			// a plan's money waits for a process that can credit it
			return true, errComputeStripeInactive
		}
		subscriptionId := stripeIdFromRaw(details.Subscription)
		switch event.Type {
		case "invoice.paid":
			return true, self.monthlyPaid(ctx, invoice, subscriptionId)
		case "invoice.payment_failed":
			return true, sidecarcontroller.ComputeMonthlyPaymentFailed(self.deps.Env(), ctx, subscriptionId, invoice.Id)
		}
		return true, nil
	case "customer.subscription.deleted":
		subscription := &struct {
			Id       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		}{}
		if err := json.Unmarshal(event.Data.Object, subscription); err != nil || !computeStripeMarked(subscription.Metadata) ||
			subscription.Metadata[computeStripeMetadataRenewal] != computeStripeRenewMonthly {
			return false, nil
		}
		if !self.deps.Active() {
			return true, errComputeStripeInactive
		}
		return true, sidecarcontroller.ComputeMonthlyEnded(self.deps.Env(), ctx, subscription.Id)
	}
	return false, nil
}

// The payer hash of intent's card, nil where it has none or the ledger holds no key.
func (self *ComputeServerStripe) payerHashOf(intent *stripe.PaymentIntent) ([]byte, error) {
	fingerprint := computeStripeFingerprint(intent)
	if fingerprint == "" {
		return nil, nil
	}
	payerHash, err := sidecarcontroller.ComputePayerHash(self.deps.Secrets(), sidecarcontroller.ComputePayerStripeCard, fingerprint)
	if errors.Is(err, sidecarcontroller.ErrComputeLedgerSecretsUnset) {
		// unkeyed, the money is final all the same: credited, counted to no payer
		return nil, nil
	}
	return payerHash, err
}

// Refunds intent's payment of a closed account, which credits nothing (§9.5), under a key of its id.
func (self *ComputeServerStripe) refundClosed(ctx context.Context, client *stripe.Client, intentId string) error {
	sidecarcontroller.ComputeRaiseAlarm(self.deps.Env(), ComputeAlarmPurchaseClosedRefunded)
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(intentId),
		Metadata:      map[string]string{ComputeStripeMetadataRefund: "closed"},
	}
	params.SetIdempotencyKey("compute_refund_closed_" + intentId)
	if _, err := client.V1Refunds.Create(ctx, params); err != nil {
		return fmt.Errorf("compute stripe: the refund of %s: %w", intentId, err)
	}
	return nil
}

// payment_intent.succeeded of a top-up (see the file header).
func (self *ComputeServerStripe) topUpSucceeded(ctx context.Context, intentId string) error {
	if !self.deps.Active() {
		return errComputeStripeInactive
	}
	client, err := self.client()
	if err != nil {
		return err
	}
	intent, err := self.intent(ctx, client, intentId)
	if err != nil {
		return err
	}
	if intent.Status != stripe.PaymentIntentStatusSucceeded {
		return nil
	}
	env := self.deps.Env()
	renewal, err := computeStripeRenewalOf(self.deps.Ledger, intent.Metadata, computeStripeRenewThreshold)
	if err != nil || intent.Currency != stripe.Currency(computeStripeCurrency) || intent.AmountReceived < renewal.credits*computeStripeCentsPerCredit {
		// taken, and nothing of ours names what it bought: a person refunds it; a retry does no better
		sidecarcontroller.ComputeRaiseAlarm(env, ComputeAlarmPurchaseMismatch)
		glog.Errorf("[compute]stripe top-up %s names no top-up of ours\n", intent.ID)
		return nil
	}
	payerHash, err := self.payerHashOf(intent)
	if err != nil {
		return err
	}
	_, err = sidecarcontroller.ComputeTopUpCredit(env, ctx, self.deps.Ledger, &sidecarcontroller.ComputeTopUpCreditRequest{
		NetworkId: renewal.networkId, PaymentIntentId: intent.ID,
		AmountNanoUsd: renewal.credits * sidecarmodel.ComputeCreditNanoUsd, PayerHash: payerHash,
	})
	if errors.Is(err, sidecarcontroller.ErrComputeAccountClosed) {
		return self.refundClosed(ctx, client, intent.ID)
	}
	return err
}

// checkout.session.completed of a plan (see the file header).
func (self *ComputeServerStripe) monthlySubscribed(ctx context.Context, raw json.RawMessage) error {
	session := &struct {
		Mode         string            `json:"mode"`
		Metadata     map[string]string `json:"metadata"`
		Subscription json.RawMessage   `json:"subscription"`
		Customer     json.RawMessage   `json:"customer"`
	}{}
	if err := json.Unmarshal(raw, session); err != nil || session.Mode != computeStripeSessionModeSubsc {
		return nil
	}
	if !self.deps.Active() {
		return errComputeStripeInactive
	}
	env := self.deps.Env()
	renewal, err := computeStripeRenewalOf(self.deps.Ledger, session.Metadata, computeStripeRenewMonthly)
	subscriptionId, customerId := stripeIdFromRaw(session.Subscription), stripeIdFromRaw(session.Customer)
	if err != nil || subscriptionId == "" || customerId == "" {
		sidecarcontroller.ComputeRaiseAlarm(env, ComputeAlarmPurchaseMismatch)
		return nil
	}
	return sidecarcontroller.ComputeMonthlySubscribed(env, ctx, self.deps.Ledger, &sidecarcontroller.ComputeMonthlySubscription{
		NetworkId: renewal.networkId, PayerMemberId: renewal.memberId, Credits: renewal.credits,
		StripeCustomerId: customerId, StripeSubscriptionId: subscriptionId,
	})
}

// The PaymentIntent that paid invoiceId: its paid payment's, "" where none is listed yet.
func (self *ComputeServerStripe) invoiceIntent(ctx context.Context, client *stripe.Client, invoiceId string) (string, error) {
	params := &stripe.InvoicePaymentListParams{Invoice: stripe.String(invoiceId)}
	for payment, err := range client.V1InvoicePayments.List(ctx, params) {
		if err != nil {
			return "", fmt.Errorf("compute stripe: the payments of %s: %w", invoiceId, err)
		}
		if payment.Status == "paid" && payment.Payment != nil && payment.Payment.PaymentIntent != nil && payment.Payment.PaymentIntent.ID != "" {
			return payment.Payment.PaymentIntent.ID, nil
		}
	}
	return "", nil
}

// invoice.paid of a plan (see the file header).
func (self *ComputeServerStripe) monthlyPaid(ctx context.Context, invoice *computeStripeInvoiceObject, subscriptionId string) error {
	env := self.deps.Env()
	renewal, err := computeStripeRenewalOf(self.deps.Ledger, invoice.Parent.SubscriptionDetails.Metadata, computeStripeRenewMonthly)
	if err != nil || invoice.Currency != computeStripeCurrency || invoice.Subtotal < renewal.credits*computeStripeCentsPerCredit {
		sidecarcontroller.ComputeRaiseAlarm(env, ComputeAlarmPurchaseMismatch)
		glog.Errorf("[compute]stripe invoice %s names no plan of ours\n", invoice.Id)
		return nil
	}
	client, err := self.client()
	if err != nil {
		return err
	}
	intentId, err := self.invoiceIntent(ctx, client, invoice.Id)
	if err != nil {
		return err
	}
	if intentId == "" {
		// paid by no PaymentIntent listed yet: Stripe delivers again
		return fmt.Errorf("compute stripe: the paid invoice %s lists no payment yet", invoice.Id)
	}
	intent, err := self.intent(ctx, client, intentId)
	if err != nil {
		return err
	}
	payerHash, err := self.payerHashOf(intent)
	if err != nil {
		return err
	}
	_, err = sidecarcontroller.ComputeMonthlyGrant(env, ctx, self.deps.Ledger, &sidecarcontroller.ComputeMonthlyGrantRequest{
		NetworkId: renewal.networkId, PaymentIntentId: intentId, Credits: renewal.credits, PayerHash: payerHash,
	})
	if errors.Is(err, sidecarcontroller.ErrComputeAccountClosed) {
		if subscriptionId != "" {
			if err := self.CancelSubscription(ctx, subscriptionId); err != nil {
				return err
			}
		}
		return self.refundClosed(ctx, client, intentId)
	}
	return err
}
