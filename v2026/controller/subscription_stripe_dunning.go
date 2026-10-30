// Failed-payment follow-up (dunning) for Stripe subscriptions.
//
// A Stripe renewal is charged off-session. When the charge fails (insufficient
// funds, a decline, a Radar block), Stripe's Smart Retries try the invoice again
// for a while and then apply the final action set in the dashboard (today:
// cancel the subscription). Many Stripe customers have no email in Stripe, so
// Stripe's own failed-payment emails cannot reach them; the account's email is
// in our database. So on invoice.payment_failed the server writes to the
// account itself (owner request, 2026-10-26: ask the customer to update their
// payment info, never let the subscription lapse without trying):
//
//   - the first notice when a renewal first fails, and the final notice when
//     Stripe's retries are about to run out (stripeDunningFinalDue). An invoice
//     owes at most these two; each links the invoice's Stripe-hosted page and
//     ur.io's Manage Subscription (StripeUpdatePaymentMethodUrl), whose
//     "Update payment method" opens the billing portal.
//   - the decision is made on the invoice's row in the failed-renewal ledger
//     (model/stripe_invoice_payment_failure_model.go), locked for the
//     transaction that also adds the notice to the account message outbox. The
//     handler reads the invoice's current state from Stripe rather than trusting
//     the event, so a redelivered, late or out-of-order event decides on what is
//     true now: a paid, voided or uncollectible invoice, or a subscription that
//     ended, owes nothing.
//   - invoice.paid, invoice.voided, invoice.marked_uncollectible and
//     customer.subscription.deleted resolve the row and withdraw the notices it
//     still has in the outbox, so a reminder never follows the payment.
//   - an account with no email (a wallet, seedphrase or phone account) gets no
//     email. The open row is the in-app past-due notice
//     (SubscriptionBalanceResult.PaymentPastDue) every app shows, with the same
//     link.
//
// The billing portal opens for the caller's own network only
// (stripeCustomerIdForNetwork): the customer is found from what the server
// recorded for that network, never from the request.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// ur.io's Manage Subscription. Its "Update payment method" opens a billing
// portal session made for the signed-in network; the emails and the apps link
// here because a portal session link expires minutes after it is made.
const StripeUpdatePaymentMethodUrl = "https://ur.io/app/subscription"

// Stripe's hosted invoice pages. A notice carries the invoice's link only when
// it is on this domain.
const stripeHostedInvoiceUrlPrefix = "https://invoice.stripe.com/"

// The billing reason of an invoice that renews a subscription period, which
// Stripe charges off-session. A subscription's first invoice is paid on-session
// in the checkout or the payment sheet, which shows its failure.
const stripeBillingReasonSubscriptionCycle = "subscription_cycle"

// The notice a failed renewal owes.
type stripePaymentFailedNotice string

const (
	stripePaymentFailedNoticeNone  stripePaymentFailedNotice = ""
	stripePaymentFailedNoticeFirst stripePaymentFailedNotice = "first"
	stripePaymentFailedNoticeFinal stripePaymentFailedNotice = "final"
)

// The dunning's tunables. The retry fields mirror the Smart Retries settings in
// the Stripe dashboard (Billing, Revenue recovery, Retries); see the doc comment
// of each for what changes when they differ.
type stripeDunningSettings struct {
	// The attempts Stripe makes on a renewal invoice before its final action:
	// the first charge plus the Smart Retries retry count. 9 (8 retries) as the
	// account is set on 2026-10-26. The final notice is due once the next
	// attempt is the last. Non-positive disables this rule.
	MaxAttemptCount int
	// The Smart Retries window, counted from the invoice's creation. 2 weeks as
	// the account is set on 2026-10-26. Non-positive disables the window rule.
	RetryWindow time.Duration
	// The final notice is due once the next attempt falls within this long of the
	// end of the retry window, so the customer gets it while a retry can still
	// succeed, even when Smart Retries make fewer attempts than the count allows.
	// Non-positive disables the window rule.
	FinalNoticeLead time.Duration
	// How long after its latest recorded failure an open failure still shows the
	// in-app past-due notice. Every failure and every resolution arrives by
	// webhook; this ends the notice when an event that would resolve it never
	// arrived. Stripe retries a few days apart at most.
	PastDueNoticeMaxAge time.Duration
	// Bounds each Stripe API read the dunning makes.
	StripeReadTimeout time.Duration
	// Where the billing portal returns the customer.
	PortalReturnUrl string
}

func defaultStripeDunningSettings() *stripeDunningSettings {
	return &stripeDunningSettings{
		MaxAttemptCount:     9,
		RetryWindow:         14 * 24 * time.Hour,
		FinalNoticeLead:     4 * 24 * time.Hour,
		PastDueNoticeMaxAge: 7 * 24 * time.Hour,
		StripeReadTimeout:   15 * time.Second,
		PortalReturnUrl:     StripeUpdatePaymentMethodUrl,
	}
}

// The settings and the clock the dunning runs with. Replaceable only by
// hermetic tests; production never mutates them.
var stripeDunningSettingsFunc = defaultStripeDunningSettings
var stripeDunningNow = server.NowUtc

// The database writes the webhooks resolve failed renewals with. Replaceable
// only by hermetic tests (no database in that env), which drive invoice.paid
// through StripeWebhook; production never mutates them.
var stripeResolveInvoicePaymentFailureFunc = stripeResolveInvoicePaymentFailure
var stripeResolveSubscriptionPaymentFailuresFunc = stripeResolveSubscriptionPaymentFailures

// The fields of a Stripe invoice the dunning reads. The subscription id is read
// in both API shapes (stripeInvoiceSubscriptionRef).
type stripeDunningInvoice struct {
	Id            string `json:"id"`
	Status        string `json:"status"`
	BillingReason string `json:"billing_reason"`
	AttemptCount  int    `json:"attempt_count"`
	// unix seconds; null when Stripe will not retry again
	NextPaymentAttempt *int64          `json:"next_payment_attempt"`
	HostedInvoiceUrl   string          `json:"hosted_invoice_url"`
	AmountDue          int64           `json:"amount_due"`
	Currency           string          `json:"currency"`
	Created            int64           `json:"created"`
	Customer           json.RawMessage `json:"customer"`
	stripeInvoiceSubscriptionRef
}

// The next automatic attempt, or nil when there is none.
func (self *stripeDunningInvoice) nextAttemptTime() *time.Time {
	if self.NextPaymentAttempt == nil || *self.NextPaymentAttempt <= 0 {
		return nil
	}
	t := time.Unix(*self.NextPaymentAttempt, 0).UTC()
	return &t
}

// The fields of a Stripe subscription the dunning reads.
type stripeDunningSubscription struct {
	Id       string            `json:"id"`
	Status   string            `json:"status"`
	Customer json.RawMessage   `json:"customer"`
	Metadata map[string]string `json:"metadata"`
}

// Whether Stripe is retrying a failed renewal of the subscription: past_due
// while Smart Retries run, unpaid when the final action keeps it unpaid.
// canceled (the final action today), incomplete and the rest owe no notice.
func stripeSubscriptionRetrying(status string) bool {
	switch status {
	case "past_due", "unpaid":
		return true
	}
	return false
}

// Reads the invoice from Stripe within the read bound.
func stripeDunningFetchInvoice(ctx context.Context, invoiceId string, settings *stripeDunningSettings) (*stripeDunningInvoice, error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.StripeReadTimeout)
	defer cancel()
	invoice, err := server.HttpGetRequireStatusOk[*stripeDunningInvoice](
		readCtx,
		fmt.Sprintf("%s/v1/invoices/%s", stripeApiBaseUrl, url.PathEscape(invoiceId)),
		stripeAuthHeader,
		server.ResponseJsonObject[*stripeDunningInvoice],
	)
	if err != nil {
		return nil, fmt.Errorf("invoice %s: %w", invoiceId, err)
	}
	if invoice == nil || invoice.Id != invoiceId {
		return nil, fmt.Errorf("invoice %s: Stripe returned another invoice", invoiceId)
	}
	return invoice, nil
}

// Reads the subscription from Stripe within the read bound.
func stripeDunningFetchSubscription(ctx context.Context, subscriptionId string, settings *stripeDunningSettings) (*stripeDunningSubscription, error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.StripeReadTimeout)
	defer cancel()
	sub, err := server.HttpGetRequireStatusOk[*stripeDunningSubscription](
		readCtx,
		fmt.Sprintf("%s/v1/subscriptions/%s", stripeApiBaseUrl, url.PathEscape(subscriptionId)),
		stripeAuthHeader,
		server.ResponseJsonObject[*stripeDunningSubscription],
	)
	if err != nil {
		return nil, fmt.Errorf("subscription %s: %w", subscriptionId, err)
	}
	if sub == nil || sub.Id != subscriptionId || sub.Status == "" {
		return nil, fmt.Errorf("subscription %s: Stripe returned another or a malformed subscription", subscriptionId)
	}
	return sub, nil
}

// The network the subscription bills, by the rules that credit its renewals
// (stripeResolveInvoiceCredit): its network_id metadata, then the
// client_reference_id of the checkout session that started it. A customer the
// server made for a network (stripe_customer) names that network last. Nil when
// nothing names one.
func stripeDunningNetworkId(ctx context.Context, sub *stripeDunningSubscription, customerId string) (*server.Id, error) {
	if subNetworkId := sub.Metadata[stripeMetadataNetworkId]; subNetworkId != "" {
		networkId, err := server.ParseId(subNetworkId)
		if err != nil {
			return nil, fmt.Errorf("subscription %s: unparseable network_id metadata: %w", sub.Id, err)
		}
		return &networkId, nil
	}
	networkId, err := stripeCheckoutSessionNetworkId(ctx, sub.Id)
	if err != nil {
		return nil, err
	}
	if networkId != nil {
		return networkId, nil
	}
	return model.GetStripeCustomerNetworkId(ctx, customerId), nil
}

// Whether the final notice is due for this failure of the invoice: the next
// attempt is Stripe's last by count, it falls in the last part of the retry
// window, or Stripe will not retry again at all.
func stripeDunningFinalDue(invoice *stripeDunningInvoice, settings *stripeDunningSettings) bool {
	nextAttemptTime := invoice.nextAttemptTime()
	if nextAttemptTime == nil {
		return true
	}
	if 0 < settings.MaxAttemptCount && settings.MaxAttemptCount <= invoice.AttemptCount+1 {
		return true
	}
	if 0 < settings.RetryWindow && 0 < settings.FinalNoticeLead && 0 < invoice.Created {
		windowEnd := time.Unix(invoice.Created, 0).UTC().Add(settings.RetryWindow)
		if !nextAttemptTime.Before(windowEnd.Add(-settings.FinalNoticeLead)) {
			return true
		}
	}
	return false
}

// The notice the failure owes, given what its ledger row (nil before the first
// record) already owed. Pure: the caller has checked the invoice is a failing
// renewal (stripeDunningFailingReason).
func stripePaymentFailedNoticeDue(
	invoice *stripeDunningInvoice,
	recorded *model.StripeInvoicePaymentFailure,
	settings *stripeDunningSettings,
) stripePaymentFailedNotice {
	if recorded != nil && (recorded.ResolveTime != nil || recorded.FinalNoticeTime != nil) {
		return stripePaymentFailedNoticeNone
	}
	if stripeDunningFinalDue(invoice, settings) {
		return stripePaymentFailedNoticeFinal
	}
	if recorded != nil && recorded.FirstNoticeTime != nil {
		return stripePaymentFailedNoticeNone
	}
	return stripePaymentFailedNoticeFirst
}

// Why the invoice is not a failing renewal the dunning follows, or "" when it
// is: an off-session renewal, still open after a failed attempt, whose
// subscription Stripe is still retrying.
func stripeDunningFailingReason(invoice *stripeDunningInvoice, sub *stripeDunningSubscription) string {
	switch {
	case invoice.BillingReason != stripeBillingReasonSubscriptionCycle:
		return "not a renewal: billing reason " + invoice.BillingReason
	case invoice.Status != "open":
		return "invoice is " + invoice.Status
	case invoice.AttemptCount < 1:
		return "no failed attempt"
	case sub == nil:
		return "no subscription"
	case !stripeSubscriptionRetrying(sub.Status):
		return "subscription is " + sub.Status
	}
	return ""
}

// The resolve reason for an invoice that left the open state, or "".
func stripeDunningResolveReason(invoiceStatus string) string {
	switch invoiceStatus {
	case "paid":
		return model.StripeInvoicePaymentFailureResolvePaid
	case "void":
		return model.StripeInvoicePaymentFailureResolveVoided
	case "uncollectible":
		return model.StripeInvoicePaymentFailureResolveUncollectible
	}
	return ""
}

// The invoice's Stripe-hosted page when it is on Stripe's invoice domain, else "".
func stripeHostedInvoiceUrlAllowed(hostedInvoiceUrl string) string {
	if !strings.HasPrefix(hostedInvoiceUrl, stripeHostedInvoiceUrlPrefix) {
		return ""
	}
	if _, err := url.Parse(hostedInvoiceUrl); err != nil {
		return ""
	}
	return hostedInvoiceUrl
}

// Stripe's currencies without a minor unit (amounts are whole units).
var stripeZeroDecimalCurrencies = map[string]bool{
	"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true, "kmf": true, "krw": true, "mga": true,
	"pyg": true, "rwf": true, "ugx": true, "vnd": true, "vuv": true, "xaf": true, "xof": true, "xpf": true,
}

// A Stripe amount in the currency's minor unit as text: "$40.00" in US dollars,
// "40.00 EUR" in another currency with cents, "4000 JPY" in one without.
func stripeFormatAmount(amount int64, currency string) string {
	currency = strings.ToLower(strings.TrimSpace(currency))
	sign := ""
	if amount < 0 {
		sign = "-"
		amount = -amount
	}
	if stripeZeroDecimalCurrencies[currency] {
		return fmt.Sprintf("%s%d %s", sign, amount, strings.ToUpper(currency))
	}
	units := fmt.Sprintf("%d.%02d", amount/100, amount%100)
	if currency == "usd" || currency == "" {
		return sign + "$" + units
	}
	return fmt.Sprintf("%s%s %s", sign, units, strings.ToUpper(currency))
}

// The outbox key of the invoice's notice.
func stripePaymentFailedNoticeKey(invoiceId string, notice stripePaymentFailedNotice) string {
	return fmt.Sprintf("%s/%s", invoiceId, notice)
}

// What processing one failed renewal decided.
type stripeDunningOutcome struct {
	InvoiceId      string
	SubscriptionId string
	CustomerId     string
	NetworkId      *server.Id
	AmountDue      int64
	Currency       string
	AttemptCount   int
	NextAttemptAt  *time.Time
	// the notice owed now (or, in a dry run, that would be)
	Notice stripePaymentFailedNotice
	// the account has an email, so the notice goes to the outbox
	HasEmail bool
	// why no notice is owed, when none is
	Skip string
}

// Processes the invoice's current state, read from Stripe: a failing renewal is
// recorded in the ledger, and the notice it is due is added to the outbox in the
// same transaction; an invoice that left the open state resolves its failure.
// With dryRun nothing is written: the outcome is what a real run would do now.
// An error means Stripe or the database could not be read; a webhook answers it
// with a non-2xx, so Stripe delivers the event again.
func stripeDunningProcessInvoice(
	ctx context.Context,
	invoiceId string,
	now time.Time,
	settings *stripeDunningSettings,
	dryRun bool,
) (*stripeDunningOutcome, error) {
	outcome := &stripeDunningOutcome{InvoiceId: invoiceId}

	invoice, err := stripeDunningFetchInvoice(ctx, invoiceId, settings)
	if err != nil {
		return nil, err
	}
	outcome.AmountDue = invoice.AmountDue
	outcome.Currency = invoice.Currency
	outcome.AttemptCount = invoice.AttemptCount
	outcome.NextAttemptAt = invoice.nextAttemptTime()
	outcome.CustomerId = stripeIdFromRaw(invoice.Customer)
	outcome.SubscriptionId = invoice.subscriptionId()

	if resolveReason := stripeDunningResolveReason(invoice.Status); resolveReason != "" {
		// paid, voided or uncollectible since: nothing is owed, and a failure on
		// file is over (its own event may still be on the way)
		outcome.Skip = "invoice is " + invoice.Status
		if !dryRun {
			stripeResolveInvoicePaymentFailure(ctx, invoiceId, resolveReason, now)
		}
		return outcome, nil
	}
	if invoice.BillingReason != stripeBillingReasonSubscriptionCycle || outcome.SubscriptionId == "" {
		outcome.Skip = stripeDunningFailingReason(invoice, nil)
		return outcome, nil
	}

	sub, err := stripeDunningFetchSubscription(ctx, outcome.SubscriptionId, settings)
	if err != nil {
		return nil, err
	}
	if outcome.CustomerId == "" {
		outcome.CustomerId = stripeIdFromRaw(sub.Customer)
	}
	if skip := stripeDunningFailingReason(invoice, sub); skip != "" {
		outcome.Skip = skip
		return outcome, nil
	}

	networkId, err := stripeDunningNetworkId(ctx, sub, outcome.CustomerId)
	if err != nil {
		return nil, err
	}
	if networkId == nil {
		// the same rule as an unresolvable paid renewal: never guess an account
		outcome.Skip = "no network: the subscription names none"
		return outcome, nil
	}
	outcome.NetworkId = networkId

	failure := &model.StripeInvoicePaymentFailure{
		InvoiceId:        invoiceId,
		NetworkId:        *networkId,
		CustomerId:       outcome.CustomerId,
		SubscriptionId:   outcome.SubscriptionId,
		HostedInvoiceUrl: stripeHostedInvoiceUrlAllowed(invoice.HostedInvoiceUrl),
		AttemptCount:     invoice.AttemptCount,
	}

	if dryRun {
		recorded := model.GetStripeInvoicePaymentFailure(ctx, invoiceId)
		outcome.Notice = stripePaymentFailedNoticeDue(invoice, recorded, settings)
		server.Tx(ctx, func(tx server.PgTx) {
			outcome.HasEmail = model.GetAccountEmailInTx(ctx, tx, *networkId) != ""
		}, server.TxReadCommitted)
		if outcome.Notice == stripePaymentFailedNoticeNone {
			outcome.Skip = stripeDunningNoticeSkip(recorded)
		}
		return outcome, nil
	}

	server.Tx(ctx, func(tx server.PgTx) {
		outcome.Notice = stripePaymentFailedNoticeNone
		outcome.HasEmail = false
		outcome.Skip = ""

		recorded := model.RecordStripeInvoicePaymentFailureInTx(ctx, tx, failure, now)
		if recorded == nil {
			server.Raise(fmt.Errorf("invoice %s: the failure was not recorded", invoiceId))
		}
		if recorded.NetworkId != *networkId {
			// the first record fixed the network; a subscription whose metadata
			// changed since does not move the notices
			glog.Warningf("[stripe]dunning: invoice %s recorded for network %s, now names %s; keeping the recorded network\n", invoiceId, recorded.NetworkId, *networkId)
		}
		outcome.NetworkId = &recorded.NetworkId

		notice := stripePaymentFailedNoticeDue(invoice, recorded, settings)
		if notice == stripePaymentFailedNoticeNone {
			outcome.Skip = stripeDunningNoticeSkip(recorded)
			return
		}
		switch notice {
		case stripePaymentFailedNoticeFinal:
			model.SetStripeInvoicePaymentFailureFinalNoticeInTx(ctx, tx, invoiceId, now)
		default:
			model.SetStripeInvoicePaymentFailureFirstNoticeInTx(ctx, tx, invoiceId, now)
		}
		outcome.Notice = notice

		email := model.GetAccountEmailInTx(ctx, tx, recorded.NetworkId)
		if email == "" {
			// a wallet, seedphrase or phone account: the in-app notice only
			return
		}
		outcome.HasEmail = true
		addAccountMessageInTx(ctx, tx, &accountMessage{
			key:       stripePaymentFailedNoticeKey(invoiceId, notice),
			networkId: &recorded.NetworkId,
			userAuth:  email,
			template: &SubscriptionPaymentFailedTemplate{
				Final:            notice == stripePaymentFailedNoticeFinal,
				HostedInvoiceUrl: failure.HostedInvoiceUrl,
				AmountDue:        invoice.AmountDue,
				Currency:         invoice.Currency,
				NextAttemptTime:  invoice.nextAttemptTime(),
			},
		})
	}, server.TxReadCommitted)

	// the past-due notice shows at once
	clearSubscriptionDetailsCached(ctx, *outcome.NetworkId)
	return outcome, nil
}

// Why a recorded failure owes no notice now.
func stripeDunningNoticeSkip(recorded *model.StripeInvoicePaymentFailure) string {
	switch {
	case recorded == nil:
		return ""
	case recorded.ResolveTime != nil:
		return "resolved: " + recorded.ResolveReason
	case recorded.FinalNoticeTime != nil:
		return "final notice already owed"
	case recorded.FirstNoticeTime != nil:
		return "first notice already owed; the final is not due yet"
	}
	return ""
}

// Resolves the invoice's open failure and withdraws the notices it still has in
// the outbox. Nothing happens for an invoice that never failed.
func stripeResolveInvoicePaymentFailure(ctx context.Context, invoiceId string, reason string, now time.Time) {
	if invoiceId == "" {
		return
	}
	var networkId *server.Id
	server.Tx(ctx, func(tx server.PgTx) {
		networkId = model.ResolveStripeInvoicePaymentFailureInTx(ctx, tx, invoiceId, reason, now)
		if networkId != nil {
			stripeWithdrawPaymentFailedNoticesInTx(ctx, tx, invoiceId, reason, now)
		}
	}, server.TxReadCommitted)
	if networkId != nil {
		glog.Infof("[stripe]dunning: invoice %s of network %s resolved (%s)\n", invoiceId, *networkId, reason)
		clearSubscriptionDetailsCached(ctx, *networkId)
	}
}

// Resolves every open failure of the subscription and withdraws their notices.
func stripeResolveSubscriptionPaymentFailures(ctx context.Context, subscriptionId string, reason string, now time.Time) {
	if subscriptionId == "" {
		return
	}
	var invoiceIdNetworkIds map[string]server.Id
	server.Tx(ctx, func(tx server.PgTx) {
		invoiceIdNetworkIds = model.ResolveStripeSubscriptionPaymentFailuresInTx(ctx, tx, subscriptionId, reason, now)
		for invoiceId := range invoiceIdNetworkIds {
			stripeWithdrawPaymentFailedNoticesInTx(ctx, tx, invoiceId, reason, now)
		}
	}, server.TxReadCommitted)
	for invoiceId, networkId := range invoiceIdNetworkIds {
		glog.Infof("[stripe]dunning: invoice %s of network %s resolved (%s)\n", invoiceId, networkId, reason)
		clearSubscriptionDetailsCached(ctx, networkId)
	}
}

// Gives up the invoice's notices that are still unsent.
func stripeWithdrawPaymentFailedNoticesInTx(ctx context.Context, tx server.PgTx, invoiceId string, reason string, now time.Time) {
	model.WithdrawAccountMessagesInTx(
		ctx,
		tx,
		(&SubscriptionPaymentFailedTemplate{}).Name(),
		[]string{
			stripePaymentFailedNoticeKey(invoiceId, stripePaymentFailedNoticeFirst),
			stripePaymentFailedNoticeKey(invoiceId, stripePaymentFailedNoticeFinal),
		},
		now,
		"withdrawn: the failed invoice is "+reason,
	)
}

// ----- webhooks -----

// invoice.payment_failed: an attempt to charge the invoice failed. Only the
// invoice id is taken from the event; the handler reads the current state.
// Always 200 except when Stripe or the database cannot be read, so Stripe
// delivers the event again.
func stripeHandleInvoicePaymentFailed(object json.RawMessage, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
	var invoice struct {
		Id            string `json:"id"`
		BillingReason string `json:"billing_reason"`
	}
	if err := json.Unmarshal(object, &invoice); err != nil {
		glog.Warningf("[stripe]dunning: could not parse the failed invoice: %s\n", err)
		return &StripeWebhookResult{}, nil
	}
	if invoice.Id == "" {
		return &StripeWebhookResult{}, nil
	}
	if invoice.BillingReason != "" && invoice.BillingReason != stripeBillingReasonSubscriptionCycle {
		// a first charge in the checkout or the payment sheet, or a one-time
		// invoice: the customer saw the failure there
		return &StripeWebhookResult{}, nil
	}
	outcome, err := stripeDunningProcessInvoice(clientSession.Ctx, invoice.Id, stripeDunningNow(), stripeDunningSettingsFunc(), false)
	if err != nil {
		glog.Errorf("[stripe]dunning: invoice %s: %s\n", invoice.Id, err)
		return nil, err
	}
	stripeDunningLogOutcome(outcome)
	return &StripeWebhookResult{}, nil
}

// invoice.voided and invoice.marked_uncollectible: the invoice will not be
// collected, so its failure is over. Always 200 unless the database fails.
func stripeHandleInvoiceResolved(object json.RawMessage, reason string, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
	var invoice struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(object, &invoice); err != nil {
		glog.Warningf("[stripe]dunning: could not parse the %s invoice: %s\n", reason, err)
		return &StripeWebhookResult{}, nil
	}
	stripeResolveInvoicePaymentFailureFunc(clientSession.Ctx, invoice.Id, reason, stripeDunningNow())
	return &StripeWebhookResult{}, nil
}

// customer.subscription.deleted: the subscription ended (Stripe's final action
// after the retries, or a cancel), so its failures are over and paying their
// invoices would not bring it back.
func stripeResolveDeletedSubscriptionPaymentFailures(object json.RawMessage, clientSession *session.ClientSession) {
	var sub struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(object, &sub); err != nil {
		glog.Warningf("[stripe]dunning: could not parse the deleted subscription: %s\n", err)
		return
	}
	stripeResolveSubscriptionPaymentFailuresFunc(clientSession.Ctx, sub.Id, model.StripeInvoicePaymentFailureResolveSubscriptionEnded, stripeDunningNow())
}

// Logs what one failed renewal decided. The address is never logged.
func stripeDunningLogOutcome(outcome *stripeDunningOutcome) {
	networkId := "-"
	if outcome.NetworkId != nil {
		networkId = outcome.NetworkId.String()
	}
	if outcome.Notice == stripePaymentFailedNoticeNone {
		glog.Infof("[stripe]dunning: invoice %s (network %s, attempt %d): no notice: %s\n", outcome.InvoiceId, networkId, outcome.AttemptCount, outcome.Skip)
		return
	}
	glog.Infof(
		"[stripe]dunning: invoice %s (network %s, attempt %d): %s notice owed, email=%t\n",
		outcome.InvoiceId, networkId, outcome.AttemptCount, outcome.Notice, outcome.HasEmail,
	)
}

// ----- the in-app past-due notice -----

// A store could not collect a renewal and is retrying it
// (SubscriptionBalanceResult.PaymentPastDue). Apps show the past-due notice
// while it is present, with an action that opens UpdatePaymentUrl.
type PaymentPastDue struct {
	// stripe
	Store string `json:"store"`
	// when the renewal first failed
	FailTime time.Time `json:"fail_time"`
	// ur.io's Manage Subscription, where the customer updates the payment method
	UpdatePaymentUrl string `json:"update_payment_url"`
}

// The network's past-due notice, or nil: an open failed renewal whose latest
// failure is recent enough (PastDueNoticeMaxAge). Never fails the balance: a
// failed read leaves the notice out.
func paymentPastDueForNetwork(ctx context.Context, networkId server.Id, now time.Time, settings *stripeDunningSettings) (pastDue *PaymentPastDue) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[stripe]dunning: could not read the past-due state of network %s: %v\n", networkId, r)
			pastDue = nil
		}
	}()
	failure := model.GetOpenStripeInvoicePaymentFailure(ctx, networkId, now.Add(-settings.PastDueNoticeMaxAge))
	if failure == nil {
		return nil
	}
	return &PaymentPastDue{
		Store:            model.SubscriptionMarketStripe,
		FailTime:         failure.FirstFailTime,
		UpdatePaymentUrl: StripeUpdatePaymentMethodUrl,
	}
}

// ----- the billing portal customer -----

// The Stripe customer of the network, from what the server recorded for that
// network: the customer of its failing renewal, then the customer the server
// made for it (stripe_customer), then the customer of its last credited Stripe
// renewal, read from that invoice. "" when the network has none. The request
// never names a customer, so a caller can only reach its own.
func stripeCustomerIdForNetwork(ctx context.Context, networkId server.Id, now time.Time, settings *stripeDunningSettings) (string, error) {
	if failure := model.GetOpenStripeInvoicePaymentFailure(ctx, networkId, now.Add(-settings.PastDueNoticeMaxAge)); failure != nil && failure.CustomerId != "" {
		return failure.CustomerId, nil
	}
	if customerId := model.GetStripeCustomerIdForNetwork(ctx, networkId); customerId != "" {
		return customerId, nil
	}
	renewal := model.GetLastSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter, model.SubscriptionMarketStripe)
	if renewal == nil || renewal.TransactionId == "" {
		return "", nil
	}
	invoice, err := stripeDunningFetchInvoice(ctx, renewal.TransactionId, settings)
	if err != nil {
		return "", err
	}
	customerId := stripeIdFromRaw(invoice.Customer)
	if customerId == "" {
		return "", errors.New("the network's last Stripe invoice names no customer")
	}
	return customerId, nil
}

// Whether the network has a Stripe customer the billing portal can open, from
// the server's records alone (no Stripe read): the gate the apps and the web
// use before offering the portal.
func stripeNetworkHasCustomer(ctx context.Context, networkId server.Id, now time.Time, settings *stripeDunningSettings) bool {
	if failure := model.GetOpenStripeInvoicePaymentFailure(ctx, networkId, now.Add(-settings.PastDueNoticeMaxAge)); failure != nil && failure.CustomerId != "" {
		return true
	}
	if model.GetStripeCustomerIdForNetwork(ctx, networkId) != "" {
		return true
	}
	renewal := model.GetLastSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter, model.SubscriptionMarketStripe)
	return renewal != nil && renewal.TransactionId != ""
}

// ----- the backfill (bringyourctl payments stripe-dunning) -----

// What the dunning owes one subscription Stripe is retrying.
type StripeDunningReportRow struct {
	SubscriptionId     string
	SubscriptionStatus string
	InvoiceId          string
	// nil when the subscription names no network
	NetworkId    *server.Id
	HasEmail     bool
	AmountDue    int64
	Currency     string
	AttemptCount int
	// nil when Stripe will not retry again
	NextAttemptTime *time.Time
	// first, final, or "" when nothing is owed now
	Notice string
	// why nothing is owed, when nothing is
	Skip string
}

// The amount due as the notices print it.
func (self *StripeDunningReportRow) Amount() string {
	return stripeFormatAmount(self.AmountDue, self.Currency)
}

// Lists every subscription Stripe is retrying (past_due and unpaid) with what
// the dunning owes its latest invoice now. With send it owes it: each invoice
// goes through stripeDunningProcessInvoice, the processing of
// invoice.payment_failed, so this run and the webhook decide on the same ledger
// row and an invoice never gets more than its two notices. Without send nothing
// is written; Stripe and the database are only read.
func StripeDunningReport(ctx context.Context, send bool) ([]*StripeDunningReportRow, error) {
	settings := stripeDunningSettingsFunc()
	rows := []*StripeDunningReportRow{}
	for _, status := range []string{"past_due", "unpaid"} {
		subs, err := stripeListSubscriptionsByStatus(ctx, status, settings)
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			row := &StripeDunningReportRow{
				SubscriptionId:     sub.Id,
				SubscriptionStatus: sub.Status,
				InvoiceId:          sub.latestInvoiceId(),
			}
			rows = append(rows, row)
			if row.InvoiceId == "" {
				row.Skip = "no latest invoice"
				continue
			}
			outcome, err := stripeDunningProcessInvoice(ctx, row.InvoiceId, stripeDunningNow(), settings, !send)
			if err != nil {
				row.Skip = "error: " + err.Error()
				continue
			}
			row.NetworkId = outcome.NetworkId
			row.HasEmail = outcome.HasEmail
			row.AmountDue = outcome.AmountDue
			row.Currency = outcome.Currency
			row.AttemptCount = outcome.AttemptCount
			row.NextAttemptTime = outcome.NextAttemptAt
			row.Notice = string(outcome.Notice)
			row.Skip = outcome.Skip
			if send {
				stripeDunningLogOutcome(outcome)
			}
		}
	}
	return rows, nil
}

// A subscription in a status listing, with its latest invoice's id.
type stripeDunningListedSubscription struct {
	stripeDunningSubscription
	LatestInvoice json.RawMessage `json:"latest_invoice"`
}

func (self *stripeDunningListedSubscription) latestInvoiceId() string {
	return stripeIdFromRaw(self.LatestInvoice)
}

// Every subscription in the status, across validated list pages.
func stripeListSubscriptionsByStatus(ctx context.Context, status string, settings *stripeDunningSettings) ([]*stripeDunningListedSubscription, error) {
	type subscriptionList struct {
		Data    []*stripeDunningListedSubscription `json:"data"`
		HasMore bool                               `json:"has_more"`
	}
	subs := []*stripeDunningListedSubscription{}
	seenSubscriptionIds := map[string]bool{}
	startingAfter := ""
	for {
		query := url.Values{
			"status": []string{status},
			"limit":  []string{"100"},
		}
		if startingAfter != "" {
			query.Set("starting_after", startingAfter)
		}
		list, err := func() (*subscriptionList, error) {
			readCtx, cancel := context.WithTimeout(ctx, settings.StripeReadTimeout)
			defer cancel()
			return server.HttpGetRequireStatusOk[*subscriptionList](
				readCtx,
				fmt.Sprintf("%s/v1/subscriptions?%s", stripeApiBaseUrl, query.Encode()),
				stripeAuthHeader,
				server.ResponseJsonObject[*subscriptionList],
			)
		}()
		if err != nil {
			return nil, fmt.Errorf("list %s subscriptions: %w", status, err)
		}
		if list == nil {
			return nil, fmt.Errorf("list %s subscriptions: no list", status)
		}
		for _, sub := range list.Data {
			if sub == nil || sub.Id == "" {
				return nil, fmt.Errorf("list %s subscriptions: a malformed subscription", status)
			}
			if seenSubscriptionIds[sub.Id] {
				return nil, fmt.Errorf("list %s subscriptions: a pagination cycle", status)
			}
			seenSubscriptionIds[sub.Id] = true
			subs = append(subs, sub)
		}
		if !list.HasMore {
			return subs, nil
		}
		if len(list.Data) == 0 {
			return nil, fmt.Errorf("list %s subscriptions: an empty continuing page", status)
		}
		startingAfter = list.Data[len(list.Data)-1].Id
	}
}
