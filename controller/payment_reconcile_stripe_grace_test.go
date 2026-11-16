package controller

// Root-cause tests of the Stripe grace (owner, 2026-11-13;
// subscription_stripe_grace.go). Every Stripe window ran the 30-day
// manual-payment grace past its period, the free trial's $0 invoice included,
// and the hourly reconcile kept that grace through unpaid and a payment_failed
// cancel without asking whether the invoice took any payment. A yearly trial
// whose first charge failed stayed Pro for 44 days without a payment: 20
// networks on 2026-11-13, none of which had paid on any rail. These drive the
// public entry points (the credit, the hourly reconcile, the webhook) against a
// fake Stripe API. Needs the test database and redis (server.DefaultTestEnv).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A Stripe window as each era of the credit wrote it.
type stripeGraceTestWindow string

const (
	// before paid_end_time was recorded: the 30-day grace, no paid end, as on
	// every window of the 2026-11-13 grace networks
	stripeGraceTestWindowLegacy stripeGraceTestWindow = "legacy"
	// the credit of 2026-11-11 to this change: the 30-day grace and the paid end
	stripeGraceTestWindowPaidEnd stripeGraceTestWindow = "paid_end"
)

// Credits the invoice's window as the credit of `era` wrote it: the period
// plus the 30-day manual-payment grace, whatever the invoice took.
func stripeGraceTestCreditWindow(
	t testing.TB,
	ctx context.Context,
	era stripeGraceTestWindow,
	networkId server.Id,
	invoiceId string,
	netRevenue model.NanoCents,
	periodStart time.Time,
	periodEnd time.Time,
) {
	credited, err := stripeCreditInvoicePaid(
		ctx, networkId, invoiceId,
		netRevenue, periodStart, periodEnd.Add(manualPaymentGracePeriod),
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, credited, true)
	if era == stripeGraceTestWindowLegacy {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE subscription_renewal SET paid_end_time = NULL WHERE network_id = $1 AND transaction_id = $2`,
				networkId,
				invoiceId,
			))
		})
	}
	connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
}

// The fake invoice of a window: its period, what it took, and its
// subscription's status and cancellation reason ("" for none).
func stripeGraceTestInvoice(
	invoiceId string,
	subscriptionId string,
	networkId server.Id,
	periodStart time.Time,
	periodEnd time.Time,
	amountPaid int64,
	subscriptionStatus string,
	reason string,
) map[string]any {
	invoice := stripeTestFullInvoice(invoiceId, subscriptionId, networkId, periodStart, periodEnd, subscriptionStatus, false)
	invoice["amount_paid"] = amountPaid
	if reason != "" {
		invoice["subscription"].(map[string]any)["cancellation_details"] = map[string]any{"reason": reason}
	}
	return invoice
}

// Runs the Stripe store of the hourly reconcile once.
func stripeGraceTestReconcile(t testing.TB, ctx context.Context) *PaymentReconcileRunResult {
	result, err := RunPaymentReconciliationWithOptions(
		reconcileTestSession(t, ctx),
		&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketStripe}},
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Errors, 0)
	return result
}

// A network that never paid loses Pro when Stripe cancels its trial's
// subscription for the failed first charge, whichever era credited its trial
// window, and when Stripe's final action keeps the subscription unpaid. Before
// the change each kept Pro to the trial's end plus 30 days.
func TestPaymentReconcileStripeNeverPaidTrialEndsAtNonPaymentCancel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)

		type trialCase struct {
			name     string
			era      stripeGraceTestWindow
			trialEnd time.Time
			status   string
			reason   string
		}
		cases := []*trialCase{
			// Smart Retries ran two weeks after the trial, then Stripe canceled
			{name: "neverpaidlegacy", era: stripeGraceTestWindowLegacy, trialEnd: now.Add(-14 * 24 * time.Hour), status: "canceled", reason: "payment_failed"},
			{name: "neverpaidpaidend", era: stripeGraceTestWindowPaidEnd, trialEnd: now.Add(-14 * 24 * time.Hour), status: "canceled", reason: "payment_failed"},
			// Stripe's final action set to mark the subscription unpaid
			{name: "neverpaidunpaid", era: stripeGraceTestWindowLegacy, trialEnd: now.Add(-14 * 24 * time.Hour), status: "unpaid"},
			// canceled inside the day after the trial: ends at the cancel, not a day later
			{name: "neverpaidquick", era: stripeGraceTestWindowPaidEnd, trialEnd: now.Add(-2 * time.Hour), status: "canceled", reason: "payment_failed"},
		}
		networkIds := map[string]server.Id{}
		for _, c := range cases {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, c.name, server.NewId())
			invoiceId := "in_synthetic_" + c.name
			trialStart := c.trialEnd.Add(-14 * 24 * time.Hour)
			stripeGraceTestCreditWindow(t, ctx, c.era, networkId, invoiceId, 0, trialStart, c.trialEnd)
			stripeEnv.fullInvoices[invoiceId] = stripeGraceTestInvoice(
				invoiceId, "sub_synthetic_"+c.name, networkId,
				trialStart, c.trialEnd, 0, c.status, c.reason,
			)
			networkIds[c.name] = networkId
		}

		result := stripeGraceTestReconcile(t, ctx)
		for _, c := range cases {
			isPro := model.IsProNetwork(ctx, networkIds[c.name])
			connect.AssertEqual(t, fmt.Sprintf("%s pro %t", c.name, isPro), fmt.Sprintf("%s pro false", c.name))
			_, inWindow := stripeTestActiveRenewalEnd(ctx, networkIds[c.name], "in_synthetic_"+c.name)
			connect.AssertEqual(t, fmt.Sprintf("%s in window %t", c.name, inWindow), fmt.Sprintf("%s in window false", c.name))
		}
		connect.AssertEqual(t, result.Ended, len(cases))
		for _, event := range model.GetPaymentReconciliationEvents(ctx, result.RunId) {
			if event.Action == model.PaymentReconcileActionEnded {
				connect.AssertEqual(t, event.Details["took_payment"], false)
			}
		}

		// the next run finds nothing left to end
		connect.AssertEqual(t, stripeGraceTestReconcile(t, ctx).Ended, 0)
	})
}

// A network that paid keeps the manual-payment grace after Stripe gives up on
// its renewal, canceled for payment_failed or kept unpaid, whichever era
// credited the window it paid for. Unchanged by the policy; this pins it.
func TestPaymentReconcileStripePaidWindowKeepsGraceAfterNonPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)

		type paidCase struct {
			name      string
			era       stripeGraceTestWindow
			periodEnd time.Time
			status    string
			reason    string
		}
		cases := []*paidCase{
			{name: "paidbeforecanceled", era: stripeGraceTestWindowLegacy, periodEnd: now.Add(-15 * 24 * time.Hour), status: "canceled", reason: "payment_failed"},
			{name: "paidbeforecanceledpaidend", era: stripeGraceTestWindowPaidEnd, periodEnd: now.Add(-15 * 24 * time.Hour), status: "canceled", reason: "payment_failed"},
			{name: "paidbeforeunpaid", era: stripeGraceTestWindowPaidEnd, periodEnd: now.Add(-15 * 24 * time.Hour), status: "unpaid"},
			// still retrying
			{name: "paidbeforepastdue", era: stripeGraceTestWindowLegacy, periodEnd: now.Add(-3 * 24 * time.Hour), status: "past_due"},
		}
		networkIds := map[string]server.Id{}
		for _, c := range cases {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, c.name, server.NewId())
			invoiceId := "in_synthetic_" + c.name
			periodStart := c.periodEnd.Add(-365 * 24 * time.Hour)
			stripeGraceTestCreditWindow(t, ctx, c.era, networkId, invoiceId, model.UsdToNanoCents(28), periodStart, c.periodEnd)
			stripeEnv.fullInvoices[invoiceId] = stripeGraceTestInvoice(
				invoiceId, "sub_synthetic_"+c.name, networkId,
				periodStart, c.periodEnd, 4000, c.status, c.reason,
			)
			networkIds[c.name] = networkId
		}

		result := stripeGraceTestReconcile(t, ctx)
		connect.AssertEqual(t, result.Ended, 0)
		for _, c := range cases {
			isPro := model.IsProNetwork(ctx, networkIds[c.name])
			connect.AssertEqual(t, fmt.Sprintf("%s pro %t", c.name, isPro), fmt.Sprintf("%s pro true", c.name))
			endTime, inWindow := stripeTestActiveRenewalEnd(ctx, networkIds[c.name], "in_synthetic_"+c.name)
			connect.AssertEqual(t, inWindow, true)
			// the whole 30 days past the paid period
			connect.AssertEqual(t, endTime.UTC().Format(time.RFC3339Nano), c.periodEnd.Add(manualPaymentGracePeriod).UTC().Format(time.RFC3339Nano))
		}
	})
}

// A cancel the customer asked for is unchanged: a paid period keeps its window
// to the period's end while its charge holds the money, and a trial canceled
// inside its trial ends at the next reconcile, as before the change.
func TestPaymentReconcileStripeRequestedCancelUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)

		// a year paid for, canceled on request a month in
		paidNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, paidNetworkId, "requestedcancelpaid", server.NewId())
		paidStart := now.Add(-30 * 24 * time.Hour)
		paidEnd := now.Add(335 * 24 * time.Hour)
		stripeGraceTestCreditWindow(t, ctx, stripeGraceTestWindowPaidEnd, paidNetworkId, "in_synthetic_requested_paid", model.UsdToNanoCents(28), paidStart, paidEnd)
		stripeEnv.fullInvoices["in_synthetic_requested_paid"] = stripeTestCanceledInvoice(
			"in_synthetic_requested_paid", "sub_synthetic_requested_paid", paidNetworkId,
			paidStart, paidEnd, "cancellation_requested", 4000, "ch_synthetic_requested_paid",
		)
		stripeEnv.charges["ch_synthetic_requested_paid"] = stripeTestCharge("ch_synthetic_requested_paid", 4000, 0, false)

		// a trial canceled on request inside the trial
		trialNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, trialNetworkId, "requestedcanceltrial", server.NewId())
		trialStart := now.Add(-5 * 24 * time.Hour)
		trialEnd := trialStart.Add(14 * 24 * time.Hour)
		stripeGraceTestCreditWindow(t, ctx, stripeGraceTestWindowPaidEnd, trialNetworkId, "in_synthetic_requested_trial", 0, trialStart, trialEnd)
		stripeEnv.fullInvoices["in_synthetic_requested_trial"] = stripeTestCanceledInvoice(
			"in_synthetic_requested_trial", "sub_synthetic_requested_trial", trialNetworkId,
			trialStart, trialEnd, "cancellation_requested", 0, "",
		)

		// a cancel at period end with time remaining: still active
		periodEndNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, periodEndNetworkId, "requestedcancelatend", server.NewId())
		stripeGraceTestCreditWindow(t, ctx, stripeGraceTestWindowPaidEnd, periodEndNetworkId, "in_synthetic_requested_at_end", model.UsdToNanoCents(3.5), paidStart, now.Add(24*time.Hour))
		stripeEnv.fullInvoices["in_synthetic_requested_at_end"] = stripeTestFullInvoice(
			"in_synthetic_requested_at_end", "sub_synthetic_requested_at_end", periodEndNetworkId,
			paidStart, now.Add(24*time.Hour), "active", true,
		)

		result := stripeGraceTestReconcile(t, ctx)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, paidNetworkId), true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, trialNetworkId), false)
		connect.AssertEqual(t, model.IsProNetwork(ctx, periodEndNetworkId), true)
		endTime, inWindow := stripeTestActiveRenewalEnd(ctx, paidNetworkId, "in_synthetic_requested_paid")
		connect.AssertEqual(t, inWindow, true)
		connect.AssertEqual(t, endTime.UTC().Format(time.RFC3339Nano), paidEnd.Add(manualPaymentGracePeriod).UTC().Format(time.RFC3339Nano))
	})
}

// A yearly trial whose first charge fails stays Pro a day after the trial, not
// 30: the credit writes the free period's day on the trial's window. A paid
// period keeps 30 days. Before the change the credit wrote 30 days on both.
func TestStripeTrialCreditRunsADayPastTheTrial(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour

		// a trial credited through the webhook's credit path, its first charge
		// failing
		trialNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, trialNetworkId, "trialcreditday", server.NewId())
		trialEnd := now.Add(-2 * time.Hour)
		trialStart := trialEnd.Add(-14 * day)
		stripeEnv.fullInvoices["in_synthetic_trial_credit_day"] = stripeGraceTestInvoice(
			"in_synthetic_trial_credit_day", "sub_synthetic_trial_credit_day", trialNetworkId,
			trialStart, trialEnd, 0, "past_due", "",
		)
		_, err := stripeHandleInvoicePaid(
			&StripeEventInvoiceObject{Id: "in_synthetic_trial_credit_day", Total: 0, Currency: "usd", BillingReason: "subscription_create"},
			reconcileTestSession(t, ctx),
		)
		connect.AssertEqual(t, err, nil)
		endTime, inWindow := stripeTestActiveRenewalEnd(ctx, trialNetworkId, "in_synthetic_trial_credit_day")
		connect.AssertEqual(t, inWindow, true)
		connect.AssertEqual(t, endTime.UTC().Format(time.RFC3339Nano), trialEnd.Add(SubscriptionGracePeriod).UTC().Format(time.RFC3339Nano))

		// a paid period keeps the 30 days
		paidNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, paidNetworkId, "paidcreditgrace", server.NewId())
		paidStart := now.Add(-2 * day)
		paidEnd := paidStart.Add(365 * day)
		stripeEnv.fullInvoices["in_synthetic_paid_credit_grace"] = stripeGraceTestInvoice(
			"in_synthetic_paid_credit_grace", "sub_synthetic_paid_credit_grace", paidNetworkId,
			paidStart, paidEnd, 4000, "active", "",
		)
		_, err = stripeHandleInvoicePaid(
			&StripeEventInvoiceObject{Id: "in_synthetic_paid_credit_grace", Total: 4000, Currency: "usd", BillingReason: "subscription_cycle"},
			reconcileTestSession(t, ctx),
		)
		connect.AssertEqual(t, err, nil)
		endTime, inWindow = stripeTestActiveRenewalEnd(ctx, paidNetworkId, "in_synthetic_paid_credit_grace")
		connect.AssertEqual(t, inWindow, true)
		connect.AssertEqual(t, endTime.UTC().Format(time.RFC3339Nano), paidEnd.Add(manualPaymentGracePeriod).UTC().Format(time.RFC3339Nano))

		// the reconcile leaves both: the trial is inside its day, the paid
		// subscription active
		connect.AssertEqual(t, stripeGraceTestReconcile(t, ctx).Ended, 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, trialNetworkId), true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, paidNetworkId), true)
	})
}

// While Stripe retries a trial's first charge (past_due), the reconcile ends a
// trial window credited before the change, with the 30-day grace, once it is a
// day past its trial, as a window credited now ends. A paid period keeps its
// 30 days while its renewal is retried. Before the change the reconcile left
// past_due alone.
func TestPaymentReconcileStripeRetriedTrialEndsADayAfterTheTrial(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour

		type retryCase struct {
			name       string
			era        stripeGraceTestWindow
			periodEnd  time.Time
			amountPaid int64
			ends       bool
		}
		cases := []*retryCase{
			{name: "trialpastduelegacy", era: stripeGraceTestWindowLegacy, periodEnd: now.Add(-30 * time.Hour), amountPaid: 0, ends: true},
			{name: "trialpastduepaidend", era: stripeGraceTestWindowPaidEnd, periodEnd: now.Add(-30 * time.Hour), amountPaid: 0, ends: true},
			// inside the day: kept until the day is over
			{name: "trialpastdueinday", era: stripeGraceTestWindowLegacy, periodEnd: now.Add(-2 * time.Hour), amountPaid: 0, ends: false},
			// a paid period keeps its 30 days while Stripe retries the renewal
			{name: "paidpastdue", era: stripeGraceTestWindowLegacy, periodEnd: now.Add(-30 * time.Hour), amountPaid: 4000, ends: false},
		}
		networkIds := map[string]server.Id{}
		for _, c := range cases {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, c.name, server.NewId())
			invoiceId := "in_synthetic_" + c.name
			periodStart := c.periodEnd.Add(-14 * day)
			netRevenue := model.NanoCents(0)
			if 0 < c.amountPaid {
				periodStart = c.periodEnd.Add(-365 * day)
				netRevenue = model.UsdToNanoCents(28)
			}
			stripeGraceTestCreditWindow(t, ctx, c.era, networkId, invoiceId, netRevenue, periodStart, c.periodEnd)
			stripeEnv.fullInvoices[invoiceId] = stripeGraceTestInvoice(
				invoiceId, "sub_synthetic_"+c.name, networkId,
				periodStart, c.periodEnd, c.amountPaid, "past_due", "",
			)
			networkIds[c.name] = networkId
		}

		result := stripeGraceTestReconcile(t, ctx)
		for _, c := range cases {
			isPro := model.IsProNetwork(ctx, networkIds[c.name])
			connect.AssertEqual(t, fmt.Sprintf("%s pro %t", c.name, isPro), fmt.Sprintf("%s pro %t", c.name, !c.ends))
		}
		connect.AssertEqual(t, result.Ended, 2)
		endTime, inWindow := stripeTestActiveRenewalEnd(ctx, networkIds["paidpastdue"], "in_synthetic_paidpastdue")
		connect.AssertEqual(t, inWindow, true)
		connect.AssertEqual(t, endTime.UTC().Format(time.RFC3339Nano), cases[3].periodEnd.Add(manualPaymentGracePeriod).UTC().Format(time.RFC3339Nano))
	})
}

// With the dashboard's "mark the invoice uncollectible" final action, Stripe
// cancels the subscription and marks its last failed invoice uncollectible:
// either event can arrive first, and Stripe delivers each at least once. The
// failure resolves once, the never-paid trial loses Pro at the reconcile after
// the cancel and stays without it, and a paid window keeps its grace, whatever
// the order.
func TestStripeMarkedUncollectibleAfterCancelIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		stripeEnv := newStripeReconcileTestEnv(t)
		clientSession := reconcileTestSession(t, ctx)
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour

		deliver := func(eventType string, object map[string]any) {
			result, err := StripeWebhook(stripeWebhookEvent(t, eventType, object), clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, result, nil)
		}
		deleted := func(subscriptionId string) map[string]any {
			return map[string]any{
				"id":                   subscriptionId,
				"object":               "subscription",
				"status":               "canceled",
				"ended_at":             now.Unix(),
				"canceled_at":          now.Unix(),
				"cancellation_details": map[string]any{"reason": "payment_failed"},
			}
		}
		uncollectible := func(invoiceId string) map[string]any {
			return map[string]any{"id": invoiceId, "object": "invoice", "status": "uncollectible"}
		}
		// the open failure of the renewal Stripe gave up on
		recordFailure := func(networkId server.Id, invoiceId string, subscriptionId string) {
			server.Tx(ctx, func(tx server.PgTx) {
				recorded := model.RecordStripeInvoicePaymentFailureInTx(ctx, tx, &model.StripeInvoicePaymentFailure{
					InvoiceId:        invoiceId,
					NetworkId:        networkId,
					CustomerId:       "cus_reconcile",
					SubscriptionId:   subscriptionId,
					HostedInvoiceUrl: "https://invoice.stripe.com/i/acct_synthetic/" + invoiceId,
					AttemptCount:     9,
				}, now.Add(-day))
				connect.AssertNotEqual(t, recorded, nil)
			})
		}

		// cancel first: a trial that never paid
		trialNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, trialNetworkId, "uncollectibletrial", server.NewId())
		trialEnd := now.Add(-14 * day)
		trialStart := trialEnd.Add(-14 * day)
		stripeGraceTestCreditWindow(t, ctx, stripeGraceTestWindowLegacy, trialNetworkId, "in_synthetic_uncollectible_trial", 0, trialStart, trialEnd)
		stripeEnv.fullInvoices["in_synthetic_uncollectible_trial"] = stripeGraceTestInvoice(
			"in_synthetic_uncollectible_trial", "sub_synthetic_uncollectible_trial", trialNetworkId,
			trialStart, trialEnd, 0, "canceled", "payment_failed",
		)
		recordFailure(trialNetworkId, "in_synthetic_uncollectible_trial_first", "sub_synthetic_uncollectible_trial")

		deliver("customer.subscription.deleted", deleted("sub_synthetic_uncollectible_trial"))
		resolved := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_uncollectible_trial_first")
		connect.AssertNotEqual(t, resolved.ResolveTime, nil)
		connect.AssertEqual(t, resolved.ResolveReason, model.StripeInvoicePaymentFailureResolveSubscriptionEnded)

		result := stripeGraceTestReconcile(t, ctx)
		connect.AssertEqual(t, result.Ended, 1)
		connect.AssertEqual(t, model.IsProNetwork(ctx, trialNetworkId), false)

		for range 2 {
			deliver("invoice.marked_uncollectible", uncollectible("in_synthetic_uncollectible_trial_first"))
			deliver("customer.subscription.deleted", deleted("sub_synthetic_uncollectible_trial"))
		}
		after := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_uncollectible_trial_first")
		connect.AssertEqual(t, after.ResolveReason, model.StripeInvoicePaymentFailureResolveSubscriptionEnded)
		connect.AssertEqual(t, after.ResolveTime.UTC().Format(time.RFC3339Nano), resolved.ResolveTime.UTC().Format(time.RFC3339Nano))
		connect.AssertEqual(t, model.IsProNetwork(ctx, trialNetworkId), false)
		connect.AssertEqual(t, stripeGraceTestReconcile(t, ctx).Ended, 0)
		connect.AssertEqual(t, model.IsProNetwork(ctx, trialNetworkId), false)

		// uncollectible first: a network that paid
		paidNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, paidNetworkId, "uncollectiblepaid", server.NewId())
		paidEnd := now.Add(-14 * day)
		paidStart := paidEnd.Add(-30 * day)
		stripeGraceTestCreditWindow(t, ctx, stripeGraceTestWindowLegacy, paidNetworkId, "in_synthetic_uncollectible_paid", model.UsdToNanoCents(3.5), paidStart, paidEnd)
		stripeEnv.fullInvoices["in_synthetic_uncollectible_paid"] = stripeGraceTestInvoice(
			"in_synthetic_uncollectible_paid", "sub_synthetic_uncollectible_paid", paidNetworkId,
			paidStart, paidEnd, 500, "canceled", "payment_failed",
		)
		recordFailure(paidNetworkId, "in_synthetic_uncollectible_paid_renewal", "sub_synthetic_uncollectible_paid")

		for range 2 {
			deliver("invoice.marked_uncollectible", uncollectible("in_synthetic_uncollectible_paid_renewal"))
		}
		resolved = model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_uncollectible_paid_renewal")
		connect.AssertNotEqual(t, resolved.ResolveTime, nil)
		connect.AssertEqual(t, resolved.ResolveReason, model.StripeInvoicePaymentFailureResolveUncollectible)
		for range 2 {
			deliver("customer.subscription.deleted", deleted("sub_synthetic_uncollectible_paid"))
		}
		after = model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_uncollectible_paid_renewal")
		connect.AssertEqual(t, after.ResolveReason, model.StripeInvoicePaymentFailureResolveUncollectible)
		connect.AssertEqual(t, after.ResolveTime.UTC().Format(time.RFC3339Nano), resolved.ResolveTime.UTC().Format(time.RFC3339Nano))

		for range 2 {
			connect.AssertEqual(t, stripeGraceTestReconcile(t, ctx).Ended, 0)
			connect.AssertEqual(t, model.IsProNetwork(ctx, paidNetworkId), true)
		}
		endTime, inWindow := stripeTestActiveRenewalEnd(ctx, paidNetworkId, "in_synthetic_uncollectible_paid")
		connect.AssertEqual(t, inWindow, true)
		connect.AssertEqual(t, endTime.UTC().Format(time.RFC3339Nano), paidEnd.Add(manualPaymentGracePeriod).UTC().Format(time.RFC3339Nano))
	})
}

// Which windows keep the manual-payment grace once Stripe gives up on the
// renewal: only one whose invoice took payment.
func TestStripeNonPaymentLeavesManualPaymentGrace(t *testing.T) {
	invoice := func(invoiceStatus string, amountPaid int64, status string, reason string) *stripeReconcileInvoiceExpanded {
		subscription := &stripeReconcileSubscription{Id: "sub_a", Status: status}
		subscription.CancellationDetails.Reason = reason
		return &stripeReconcileInvoiceExpanded{Id: "in_a", Status: invoiceStatus, AmountPaid: amountPaid, Subscription: subscription}
	}
	type graceCase struct {
		name    string
		invoice *stripeReconcileInvoiceExpanded
		keeps   bool
	}
	cases := []graceCase{
		{"paid, canceled for payment_failed", invoice("paid", 4000, "canceled", "payment_failed"), true},
		{"paid, unpaid", invoice("paid", 4000, "unpaid", ""), true},
		{"trial, canceled for payment_failed", invoice("paid", 0, "canceled", "payment_failed"), false},
		{"trial, unpaid", invoice("paid", 0, "unpaid", ""), false},
		{"paid, canceled on request", invoice("paid", 4000, "canceled", "cancellation_requested"), false},
		{"paid, canceled for a dispute", invoice("paid", 4000, "canceled", "payment_disputed"), false},
		{"paid, past_due", invoice("paid", 4000, "past_due", ""), false},
		{"paid, active", invoice("paid", 4000, "active", ""), false},
		{"invoice not paid", invoice("open", 4000, "unpaid", ""), false},
		{"no subscription", &stripeReconcileInvoiceExpanded{Id: "in_a", Status: "paid", AmountPaid: 4000}, false},
		{"no invoice", nil, false},
	}
	for _, c := range cases {
		keeps := stripeNonPaymentLeavesManualPaymentGrace(c.invoice)
		connect.AssertEqual(t, fmt.Sprintf("%s %t", c.name, keeps), fmt.Sprintf("%s %t", c.name, c.keeps))
	}
}
