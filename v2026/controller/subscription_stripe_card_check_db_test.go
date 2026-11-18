package controller

// Tests against the database of the trial card check
// (subscription_stripe_card_check.go). A payment sheet's or a checkout's
// trial whose card failed its CVC check is never credited, whichever of the
// webhooks, the purchase report and the hourly reconcile sees the card first;
// the card is detached so it is never charged; the report answers
// incorrect_cvc; a retry with a good card starts the trial; cards without a
// failed check start it as before; and no window already credited is touched
// (the owner, 2026-11-16, on the windows the reconcile had credited to
// abandoned sheets: "ok just let them run out"). Stripe is the fake api of
// subscription_stripe_card_check_fake_test.go. Needs the test database and
// redis (server.DefaultTestEnv).

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A signed-in network with its Stripe customer.
type stripeCardCheckDbNetwork struct {
	networkId     server.Id
	clientSession *session.ClientSession
	customerId    string
}

func newStripeCardCheckDbNetwork(t testing.TB, ctx context.Context, name string) *stripeCardCheckDbNetwork {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, name, userId)
	customerId := "cus_" + name
	if err := model.CreateStripeCustomerForNetwork(ctx, networkId, customerId); err != nil {
		t.Fatal(err)
	}
	return &stripeCardCheckDbNetwork{
		networkId: networkId,
		clientSession: session.NewLocalClientSession(ctx, "127.0.0.1:1", &session.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: name,
		}),
		customerId: customerId,
	}
}

// A yearly trial of the network, started a minute ago: its card answered
// cvcCheck, and its $0 trial invoice is paid. sheet: the payment sheet's
// (pending), else a checkout's.
func addStripeCardCheckTrial(fake *stripeCardCheckFake, network *stripeCardCheckDbNetwork, name string, sheet bool, cvcCheck string) {
	now := server.NowUtc()
	trialEnd := now.Add(StripeSubscriptionTrialDays * 24 * time.Hour)
	metadata := map[string]string{"plan": "yearly"}
	if sheet {
		metadata["payment_sheet"] = "pending"
	}
	fake.addCard("pm_"+name, network.customerId, cvcCheck)
	fake.addSubscription("sub_"+name, network.customerId, network.networkId, "trialing", metadata, "pm_"+name, "in_"+name, now.Add(-time.Minute), trialEnd)
	fake.addInvoice("in_"+name, "sub_"+name, 0, now.Add(-time.Minute), trialEnd)
}

// Delivers a Stripe event to the webhook.
func deliverStripeCardCheckEvent(t testing.TB, ctx context.Context, eventType string, object map[string]any) {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	_, err = StripeWebhook(&StripeWebhookArgs{
		Id:   "evt_" + server.NewId().String(),
		Type: eventType,
		Data: &StripeEventData{Object: raw},
	}, session.Testing_CreateClientSession(ctx, nil))
	if err != nil {
		t.Fatalf("%s: %v", eventType, err)
	}
}

func stripeCardCheckSetupIntent(customerId string, paymentMethodId string) map[string]any {
	return map[string]any{
		"id":             "seti_" + paymentMethodId,
		"object":         "setup_intent",
		"customer":       customerId,
		"payment_method": paymentMethodId,
		"status":         "succeeded",
	}
}

// A trial's first invoice, as invoice.paid delivers it.
func stripeCardCheckInvoicePaid(invoiceId string, customerId string) map[string]any {
	return map[string]any{
		"id":             invoiceId,
		"object":         "invoice",
		"total":          0,
		"customer":       customerId,
		"currency":       "usd",
		"billing_reason": "subscription_create",
	}
}

// A trial's first invoice, as the reconcile's listing of paid invoices has it.
func stripeCardCheckListedInvoice(invoiceId string, created time.Time) map[string]any {
	return map[string]any{
		"id":             invoiceId,
		"total":          0,
		"billing_reason": "subscription_create",
		"created":        created.Unix(),
	}
}

// The end of the window the invoice credited, nil for none.
func stripeCardCheckWindowEnd(t testing.TB, ctx context.Context, networkId server.Id, invoiceId string) *time.Time {
	t.Helper()
	var endTime *time.Time
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT end_time FROM subscription_renewal WHERE network_id = $1 AND transaction_id = $2`,
			networkId,
			invoiceId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var end time.Time
				server.Raise(result.Scan(&end))
				endTime = &end
			}
		})
	})
	return endTime
}

func stripeCardCheckRefusals(flow string) float64 {
	return testutil.ToFloat64(stripeCardCheckRefusalsTotal.WithLabelValues(flow))
}

// The payment sheet: a failed CVC is refused whichever path sees the card
// first, redeliveries change nothing, cards without a failed check start the
// trial, and a retry with a good card succeeds.
func TestStripeTrialCardCheckPaymentSheet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		fake := newStripeCardCheckFake(t)
		refusalsBefore := stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet)

		// the webhook sees the card first
		network := newStripeCardCheckDbNetwork(t, ctx, "cardchecksheet")
		addStripeCardCheckTrial(fake, network, "sheetbad", true, "fail")
		// the trial invoice is paid when the subscription is made: held
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_sheetbad", network.customerId))
		deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(network.customerId, "pm_sheetbad"))

		metadata := fake.metadata("sub_sheetbad")
		connect.AssertEqual(t, metadata["payment_sheet"], "pending")
		connect.AssertEqual(t, metadata["card_check"], "cvc_failed")
		connect.AssertEqual(t, fake.defaultPaymentMethod("sub_sheetbad"), "")
		// left pending, for a new sheet or the sweep to replace
		connect.AssertEqual(t, fake.status("sub_sheetbad"), "trialing")
		updates, detached, canceled := fake.writes()
		connect.AssertEqual(t, len(updates), 1)
		connect.AssertEqual(t, detached, []string{"pm_sheetbad"})
		connect.AssertEqual(t, len(canceled), 0)
		ledgerRows, renewalRows := stripeVerifyDbCounts(t, ctx, network.networkId, "in_sheetbad")
		connect.AssertEqual(t, ledgerRows, 0)
		connect.AssertEqual(t, renewalRows, 0)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, network.networkId), false)
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet), refusalsBefore+1)

		// the app's report answers the refusal
		result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_sheetbad"}, network.clientSession)
		requireVerifyCardRefusal(t, "report after the webhook", result, err)

		// redeliveries and the other webhooks change nothing, and count nothing
		deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(network.customerId, "pm_sheetbad"))
		deliverStripeCardCheckEvent(t, ctx, "payment_method.attached", map[string]any{
			"id":       "pm_sheetbad",
			"object":   "payment_method",
			"customer": network.customerId,
			"card":     map[string]any{"country": "DE", "checks": map[string]any{"cvc_check": "fail"}},
		})
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_sheetbad", network.customerId))
		updates, detached, _ = fake.writes()
		connect.AssertEqual(t, len(updates), 1)
		connect.AssertEqual(t, len(detached), 1)
		ledgerRows, _ = stripeVerifyDbCounts(t, ctx, network.networkId, "in_sheetbad")
		connect.AssertEqual(t, ledgerRows, 0)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, network.networkId), false)
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet), refusalsBefore+1)
		// the refused card's country is not recorded as the customer's
		connect.AssertEqual(t, model.GetStripeCustomerBillingCountry(ctx, network.networkId), "")

		// the report sees the card first: the same refusal, and the webhooks
		// after it agree
		reportFirst := newStripeCardCheckDbNetwork(t, ctx, "cardcheckreport")
		addStripeCardCheckTrial(fake, reportFirst, "reportbad", true, "fail")
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_reportbad"}, reportFirst.clientSession)
		requireVerifyCardRefusal(t, "report first", result, err)
		connect.AssertEqual(t, fake.metadata("sub_reportbad")["card_check"], "cvc_failed")
		connect.AssertEqual(t, fake.defaultPaymentMethod("sub_reportbad"), "")
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet), refusalsBefore+2)
		deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(reportFirst.customerId, "pm_reportbad"))
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_reportbad", reportFirst.customerId))
		ledgerRows, _ = stripeVerifyDbCounts(t, ctx, reportFirst.networkId, "in_reportbad")
		connect.AssertEqual(t, ledgerRows, 0)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, reportFirst.networkId), false)
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet), refusalsBefore+2)
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_reportbad"}, reportFirst.clientSession)
		requireVerifyCardRefusal(t, "report again", result, err)

		// cards whose check passed or returned no result start the trial
		for _, cvcCheck := range []string{"pass", "unavailable", "unchecked", ""} {
			name := "cardcheckok" + cvcCheck
			if cvcCheck == "" {
				name = "cardcheckoknone"
			}
			accepted := newStripeCardCheckDbNetwork(t, ctx, name)
			addStripeCardCheckTrial(fake, accepted, name, true, cvcCheck)
			deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_"+name, accepted.customerId))
			connect.AssertEqual(t, model.IsProNetworkFresh(ctx, accepted.networkId), false)
			deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(accepted.customerId, "pm_"+name))
			connect.AssertEqual(t, fake.metadata("sub_" + name)["payment_sheet"], "attached")
			connect.AssertEqual(t, fake.metadata("sub_" + name)["card_check"], "")
			connect.AssertEqual(t, fake.defaultPaymentMethod("sub_"+name), "pm_"+name)
			ledgerRows, renewalRows = stripeVerifyDbCounts(t, ctx, accepted.networkId, "in_"+name)
			connect.AssertEqual(t, ledgerRows, 1)
			connect.AssertEqual(t, renewalRows, 1)
			connect.AssertEqual(t, model.IsProNetworkFresh(ctx, accepted.networkId), true)
			result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_" + name}, accepted.clientSession)
			requireVerifyStatus(t, name, result, err, VerifyPurchaseStatusAlreadyCredited)
		}
		_, detached, _ = fake.writes()
		connect.AssertEqual(t, detached, []string{"pm_sheetbad", "pm_reportbad"})
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet), refusalsBefore+2)

		// a retry with a good card after the refusal: the refused sheet holds
		// no purchase back, the new sheet replaces it, and its trial starts
		retry := newStripeCardCheckDbNetwork(t, ctx, "cardcheckretry")
		addStripeCardCheckTrial(fake, retry, "retrybad", true, "fail")
		deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(retry.customerId, "pm_retrybad"))
		connect.AssertEqual(t, fake.metadata("sub_retrybad")["card_check"], "cvc_failed")
		connect.AssertEqual(t, stripeNetworkSubscriptionBills(ctx, retry.networkId, retry.customerId), false)
		connect.AssertEqual(t, stripeNewSubscriptionRefusal(retry.clientSession, retry.customerId), "")
		// StripePaymentSheet replaces the abandoned sheet before it makes the new one
		stripeCancelPendingPaymentSheetSubscriptions(retry.customerId, retry.networkId)
		connect.AssertEqual(t, fake.status("sub_retrybad"), "canceled")
		addStripeCardCheckTrial(fake, retry, "retrygood", true, "pass")
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_retrygood", retry.customerId))
		deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(retry.customerId, "pm_retrygood"))
		connect.AssertEqual(t, fake.metadata("sub_retrygood")["payment_sheet"], "attached")
		ledgerRows, _ = stripeVerifyDbCounts(t, ctx, retry.networkId, "in_retrygood")
		connect.AssertEqual(t, ledgerRows, 1)
		ledgerRows, _ = stripeVerifyDbCounts(t, ctx, retry.networkId, "in_retrybad")
		connect.AssertEqual(t, ledgerRows, 0)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, retry.networkId), true)
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_retrygood"}, retry.clientSession)
		requireVerifyStatus(t, "retry with a good card", result, err, VerifyPurchaseStatusAlreadyCredited)
		// the refused sheet still answers its refusal
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_retrybad"}, retry.clientSession)
		requireVerifyCardRefusal(t, "the replaced refused sheet", result, err)
	})
}

// Checkout: a failed CVC is refused and the trial's subscription canceled,
// whichever of the report and invoice.paid sees it first; a passing card
// starts the trial.
func TestStripeTrialCardCheckCheckout(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		fake := newStripeCardCheckFake(t)
		refusalsBefore := stripeCardCheckRefusals(stripeCardCheckFlowCheckout)

		// the report sees it first
		network := newStripeCardCheckDbNetwork(t, ctx, "cardcheckcheckout")
		addStripeCardCheckTrial(fake, network, "checkoutbad", false, "fail")
		fake.addCheckout("cs_test_checkoutbad", network.networkId, "sub_checkoutbad", "in_checkoutbad", network.customerId)
		result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_checkoutbad"}, network.clientSession)
		requireVerifyCardRefusal(t, "checkout report first", result, err)
		connect.AssertEqual(t, fake.status("sub_checkoutbad"), "canceled")
		connect.AssertEqual(t, fake.metadata("sub_checkoutbad")["card_check"], "cvc_failed")
		connect.AssertEqual(t, fake.defaultPaymentMethod("sub_checkoutbad"), "")
		_, detached, canceled := fake.writes()
		connect.AssertEqual(t, detached, []string{"pm_checkoutbad"})
		connect.AssertEqual(t, canceled, []string{"sub_checkoutbad"})
		ledgerRows, renewalRows := stripeVerifyDbCounts(t, ctx, network.networkId, "in_checkoutbad")
		connect.AssertEqual(t, ledgerRows, 0)
		connect.AssertEqual(t, renewalRows, 0)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, network.networkId), false)
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowCheckout), refusalsBefore+1)
		// invoice.paid after it: nothing more
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_checkoutbad", network.customerId))
		ledgerRows, _ = stripeVerifyDbCounts(t, ctx, network.networkId, "in_checkoutbad")
		connect.AssertEqual(t, ledgerRows, 0)
		_, _, canceled = fake.writes()
		connect.AssertEqual(t, len(canceled), 1)
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowCheckout), refusalsBefore+1)
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_checkoutbad"}, network.clientSession)
		requireVerifyCardRefusal(t, "checkout report again", result, err)

		// invoice.paid sees it first; the report agrees
		hookFirst := newStripeCardCheckDbNetwork(t, ctx, "cardcheckcheckouthook")
		addStripeCardCheckTrial(fake, hookFirst, "hookbad", false, "fail")
		fake.addCheckout("cs_test_hookbad", hookFirst.networkId, "sub_hookbad", "in_hookbad", hookFirst.customerId)
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_hookbad", hookFirst.customerId))
		connect.AssertEqual(t, fake.status("sub_hookbad"), "canceled")
		ledgerRows, _ = stripeVerifyDbCounts(t, ctx, hookFirst.networkId, "in_hookbad")
		connect.AssertEqual(t, ledgerRows, 0)
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_hookbad"}, hookFirst.clientSession)
		requireVerifyCardRefusal(t, "checkout webhook first", result, err)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, hookFirst.networkId), false)
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowCheckout), refusalsBefore+2)

		// a passing card starts the trial
		good := newStripeCardCheckDbNetwork(t, ctx, "cardcheckcheckoutok")
		addStripeCardCheckTrial(fake, good, "checkoutgood", false, "pass")
		fake.addCheckout("cs_test_checkoutgood", good.networkId, "sub_checkoutgood", "in_checkoutgood", good.customerId)
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_checkoutgood", good.customerId))
		ledgerRows, renewalRows = stripeVerifyDbCounts(t, ctx, good.networkId, "in_checkoutgood")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, renewalRows, 1)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, good.networkId), true)
		connect.AssertEqual(t, fake.status("sub_checkoutgood"), "trialing")
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_checkoutgood"}, good.clientSession)
		requireVerifyStatus(t, "checkout with a good card", result, err, VerifyPurchaseStatusAlreadyCredited)
		_, detached, canceled = fake.writes()
		connect.AssertEqual(t, detached, []string{"pm_checkoutbad", "pm_hookbad"})
		connect.AssertEqual(t, canceled, []string{"sub_checkoutbad", "sub_hookbad"})
	})
}

// The hourly reconcile credits a $0 trial invoice only as invoice.paid would,
// and no window already credited is touched by the reconcile, the webhooks
// or the abandoned-sheet sweep.
func TestStripeTrialCardCheckReconcileAndCreditedWindows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		defer disableAllReconcileStores(t)()
		fake := newStripeCardCheckFake(t)
		previousHasCredentials := stripeReconcileHasCredentials
		stripeReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { stripeReconcileHasCredentials = previousHasCredentials })
		now := server.NowUtc()
		reconcile := func(dryRun bool) {
			t.Helper()
			_, err := RunPaymentReconciliationWithOptions(
				reconcileTestSession(t, ctx),
				&PaymentReconcileRunOptions{DryRun: dryRun, Stores: []string{model.SubscriptionMarketStripe}},
			)
			connect.AssertEqual(t, err, nil)
		}

		// listed paid $0 invoices the ledger does not hold
		pending := newStripeCardCheckDbNetwork(t, ctx, "cardcheckrecpending")
		fake.addSubscription("sub_recpending", pending.customerId, pending.networkId, "trialing", map[string]string{"plan": "yearly", "payment_sheet": "pending"}, "", "in_recpending", now.Add(-time.Hour), now.Add(14*24*time.Hour))
		fake.addInvoice("in_recpending", "sub_recpending", 0, now.Add(-time.Hour), now.Add(14*24*time.Hour))
		refused := newStripeCardCheckDbNetwork(t, ctx, "cardcheckrecrefused")
		fake.addSubscription("sub_recrefused", refused.customerId, refused.networkId, "trialing", map[string]string{"plan": "yearly", "payment_sheet": "pending", "card_check": "cvc_failed"}, "", "in_recrefused", now.Add(-time.Hour), now.Add(14*24*time.Hour))
		fake.addInvoice("in_recrefused", "sub_recrefused", 0, now.Add(-time.Hour), now.Add(14*24*time.Hour))
		checkoutBad := newStripeCardCheckDbNetwork(t, ctx, "cardcheckrecbad")
		addStripeCardCheckTrial(fake, checkoutBad, "recbad", false, "fail")
		checkoutGood := newStripeCardCheckDbNetwork(t, ctx, "cardcheckrecgood")
		addStripeCardCheckTrial(fake, checkoutGood, "recgood", false, "pass")
		// a checkout trial whose card failed, its invoice made over a day ago:
		// the reconcile leaves a $0 invoice that old alone, unread
		checkoutOld := newStripeCardCheckDbNetwork(t, ctx, "cardcheckrecold")
		oldCreated := now.Add(-stripeReconcileTrialInvoiceMaxAge - time.Hour)
		fake.addCard("pm_recold", checkoutOld.customerId, "fail")
		fake.addSubscription("sub_recold", checkoutOld.customerId, checkoutOld.networkId, "trialing", map[string]string{"plan": "yearly"}, "pm_recold", "in_recold", oldCreated, oldCreated.Add(14*24*time.Hour))
		fake.addInvoice("in_recold", "sub_recold", 0, oldCreated, oldCreated.Add(14*24*time.Hour))
		fake.listInvoices = []map[string]any{
			stripeCardCheckListedInvoice("in_recpending", now.Add(-time.Hour)),
			stripeCardCheckListedInvoice("in_recrefused", now.Add(-time.Hour)),
			stripeCardCheckListedInvoice("in_recbad", now.Add(-time.Minute)),
			stripeCardCheckListedInvoice("in_recgood", now.Add(-time.Minute)),
			stripeCardCheckListedInvoice("in_recold", oldCreated),
		}

		// a dry run decides and writes nothing
		reconcile(true)
		updates, detached, canceled := fake.writes()
		connect.AssertEqual(t, len(updates)+len(detached)+len(canceled), 0)
		for _, network := range []*stripeCardCheckDbNetwork{pending, refused, checkoutBad, checkoutGood} {
			connect.AssertEqual(t, model.IsProNetworkFresh(ctx, network.networkId), false)
		}

		// the real run: only the passing checkout trial is credited; the
		// failed card is refused now
		reconcile(false)
		ledgerRows, _ := stripeVerifyDbCounts(t, ctx, checkoutGood.networkId, "in_recgood")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, checkoutGood.networkId), true)
		for network, invoiceId := range map[*stripeCardCheckDbNetwork]string{pending: "in_recpending", refused: "in_recrefused", checkoutBad: "in_recbad"} {
			ledgerRows, renewalRows := stripeVerifyDbCounts(t, ctx, network.networkId, invoiceId)
			connect.AssertEqual(t, ledgerRows, 0)
			connect.AssertEqual(t, renewalRows, 0)
			connect.AssertEqual(t, model.IsProNetworkFresh(ctx, network.networkId), false)
		}
		connect.AssertEqual(t, fake.status("sub_recbad"), "canceled")
		connect.AssertEqual(t, fake.metadata("sub_recbad")["card_check"], "cvc_failed")
		connect.AssertEqual(t, fake.status("sub_recpending"), "trialing")
		connect.AssertEqual(t, fake.status("sub_recrefused"), "trialing")
		_, detached, canceled = fake.writes()
		connect.AssertEqual(t, detached, []string{"pm_recbad"})
		connect.AssertEqual(t, canceled, []string{"sub_recbad"})
		// a second run does nothing more
		reconcile(false)
		updatesAfter, detachedAfter, canceledAfter := fake.writes()
		connect.AssertEqual(t, len(updatesAfter), 1)
		connect.AssertEqual(t, len(detachedAfter), 1)
		connect.AssertEqual(t, len(canceledAfter), 1)
		// the day-old invoice was never read, its trial never touched
		connect.AssertEqual(t, fake.requestCount("GET /v1/invoices/in_recold"), 0)
		connect.AssertEqual(t, fake.status("sub_recold"), "trialing")
		connect.AssertEqual(t, fake.metadata("sub_recold")["card_check"], "")

		// windows credited before the check: an abandoned sheet's trial the
		// reconcile credited before the hold, and a checkout trial whose card
		// failed its CVC check. Both run out as they are.
		abandoned := newStripeCardCheckDbNetwork(t, ctx, "cardcheckcreditedsheet")
		abandonedCreated := now.Add(-2 * 24 * time.Hour)
		fake.addSubscription("sub_creditedsheet", abandoned.customerId, abandoned.networkId, "trialing", map[string]string{"plan": "yearly", "payment_sheet": "pending"}, "", "in_creditedsheet", abandonedCreated, abandonedCreated.Add(14*24*time.Hour))
		fake.addInvoice("in_creditedsheet", "sub_creditedsheet", 0, abandonedCreated, abandonedCreated.Add(14*24*time.Hour))
		creditedCheckout := newStripeCardCheckDbNetwork(t, ctx, "cardcheckcreditedcheckout")
		addStripeCardCheckTrial(fake, creditedCheckout, "creditedcheckout", false, "fail")
		fake.addCheckout("cs_test_creditedcheckout", creditedCheckout.networkId, "sub_creditedcheckout", "in_creditedcheckout", creditedCheckout.customerId)
		// how they were credited before: the invoice.paid credit, with no hold
		// and no card check
		for _, invoiceId := range []string{"in_creditedsheet", "in_creditedcheckout"} {
			_, err := stripeHandleInvoicePaid(&StripeEventInvoiceObject{Id: invoiceId}, session.Testing_CreateClientSession(ctx, nil))
			connect.AssertEqual(t, err, nil)
		}
		sheetWindow := stripeCardCheckWindowEnd(t, ctx, abandoned.networkId, "in_creditedsheet")
		checkoutWindow := stripeCardCheckWindowEnd(t, ctx, creditedCheckout.networkId, "in_creditedcheckout")
		if sheetWindow == nil || checkoutWindow == nil {
			t.Fatalf("windows = %v %v", sheetWindow, checkoutWindow)
		}
		writesBefore := func() int {
			updates, detached, canceled := fake.writes()
			return len(updates) + len(detached) + len(canceled)
		}()

		// the reconcile lists them again, the webhooks redeliver, the report
		// asks, and the sweep runs
		fake.listInvoices = append(fake.listInvoices,
			stripeCardCheckListedInvoice("in_creditedsheet", abandonedCreated),
			stripeCardCheckListedInvoice("in_creditedcheckout", now.Add(-time.Minute)),
		)
		reconcile(false)
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_creditedsheet", abandoned.customerId))
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_creditedcheckout", creditedCheckout.customerId))
		result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SessionId: "cs_test_creditedcheckout"}, creditedCheckout.clientSession)
		requireVerifyStatus(t, "a checkout credited before the check", result, err, VerifyPurchaseStatusAlreadyCredited)
		sweep, err := stripeSweepAbandonedSheets(session.Testing_CreateClientSession(ctx, nil), now, defaultStripeAbandonedSheetSettings())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, sweep.CanceledCount, 0)
		connect.AssertEqual(t, sweep.AttachedCount, 0)
		connect.AssertEqual(t, sweep.RefusedCount, 0)
		if sweep.CreditedCount < 1 {
			t.Fatalf("the sweep did not keep the credited sheet: %+v", sweep)
		}

		// nothing was written to either subscription, and both windows stand
		updatesNow, detachedNow, canceledNow := fake.writes()
		connect.AssertEqual(t, len(updatesNow)+len(detachedNow)+len(canceledNow), writesBefore)
		connect.AssertEqual(t, slices.Contains(canceledNow, "sub_creditedsheet"), false)
		connect.AssertEqual(t, slices.Contains(canceledNow, "sub_creditedcheckout"), false)
		connect.AssertEqual(t, fake.status("sub_creditedsheet"), "trialing")
		connect.AssertEqual(t, fake.status("sub_creditedcheckout"), "trialing")
		connect.AssertEqual(t, fake.metadata("sub_creditedcheckout")["card_check"], "")
		connect.AssertEqual(t, fake.defaultPaymentMethod("sub_creditedcheckout"), "pm_creditedcheckout")
		connect.AssertEqual(t, stripeCardCheckWindowEnd(t, ctx, abandoned.networkId, "in_creditedsheet").Equal(*sheetWindow), true)
		connect.AssertEqual(t, stripeCardCheckWindowEnd(t, ctx, creditedCheckout.networkId, "in_creditedcheckout").Equal(*checkoutWindow), true)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, abandoned.networkId), true)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, creditedCheckout.networkId), true)
	})
}

// The payment sheet judges only the card on its own subscription: a
// returning customer's older default, failed or not, never stands in for the
// sheet's new card, whatever order the card reaches the customer and the
// subscription. A card that passes on a refused sheet clears the refusal, and
// a refused sheet never holds a new purchase back. A sheet whose trial was
// credited before is never refused.
func TestStripeTrialCardCheckSheetCardRaces(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		fake := newStripeCardCheckFake(t)
		now := server.NowUtc()
		trialEnd := now.Add(StripeSubscriptionTrialDays * 24 * time.Hour)
		refusalsBefore := stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet)

		// a returning customer whose default card failed its CVC check opens a
		// new sheet; the new card reaches the customer before the subscription
		returning := newStripeCardCheckDbNetwork(t, ctx, "cardcheckreturning")
		fake.addCard("pm_returningold", returning.customerId, "fail")
		fake.addCustomer(returning.customerId, "pm_returningold")
		fake.addCard("pm_returningnew", returning.customerId, "pass")
		fake.addSubscription("sub_returning", returning.customerId, returning.networkId, "trialing", map[string]string{"plan": "yearly", "payment_sheet": "pending"}, "", "in_returning", now.Add(-time.Minute), trialEnd)
		fake.addInvoice("in_returning", "sub_returning", 0, now.Add(-time.Minute), trialEnd)
		deliverStripeCardCheckEvent(t, ctx, "invoice.paid", stripeCardCheckInvoicePaid("in_returning", returning.customerId))
		deliverStripeCardCheckEvent(t, ctx, "payment_method.attached", map[string]any{
			"id":       "pm_returningnew",
			"object":   "payment_method",
			"customer": returning.customerId,
			"card":     map[string]any{"country": "DE", "checks": map[string]any{"cvc_check": "pass"}},
		})
		// nothing judged the old card: no refusal, nothing detached, still waiting
		connect.AssertEqual(t, fake.metadata("sub_returning")["card_check"], "")
		connect.AssertEqual(t, fake.metadata("sub_returning")["payment_sheet"], "pending")
		_, detached, _ := fake.writes()
		connect.AssertEqual(t, len(detached), 0)
		result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_returning"}, returning.clientSession)
		requireVerifyStatus(t, "before the card is on the subscription", result, err, VerifyPurchaseStatusPending)
		// then the card lands on the subscription: the trial starts
		fake.setSubscriptionField("sub_returning", "default_payment_method", "pm_returningnew")
		deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(returning.customerId, "pm_returningnew"))
		connect.AssertEqual(t, fake.metadata("sub_returning")["payment_sheet"], "attached")
		ledgerRows, _ := stripeVerifyDbCounts(t, ctx, returning.networkId, "in_returning")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, returning.networkId), true)
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_returning"}, returning.clientSession)
		requireVerifyStatus(t, "the returning customer's trial", result, err, VerifyPurchaseStatusAlreadyCredited)
		_, detached, _ = fake.writes()
		connect.AssertEqual(t, len(detached), 0)
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet), refusalsBefore)

		// a refused sheet, then a card that passes on the same subscription:
		// the refused sheet holds no purchase back, card or not, and the report
		// clears the refusal and starts the trial
		cleared := newStripeCardCheckDbNetwork(t, ctx, "cardcheckcleared")
		addStripeCardCheckTrial(fake, cleared, "clearedbad", true, "fail")
		deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(cleared.customerId, "pm_clearedbad"))
		connect.AssertEqual(t, fake.metadata("sub_clearedbad")["card_check"], "cvc_failed")
		connect.AssertEqual(t, stripeNewSubscriptionRefusal(cleared.clientSession, cleared.customerId), "")
		fake.addCard("pm_clearedgood", cleared.customerId, "pass")
		fake.setSubscriptionField("sub_clearedbad", "default_payment_method", "pm_clearedgood")
		connect.AssertEqual(t, stripeNewSubscriptionRefusal(cleared.clientSession, cleared.customerId), "")
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_clearedbad"}, cleared.clientSession)
		requireVerifyStatus(t, "a refused sheet with a good card since", result, err, VerifyPurchaseStatusCredited)
		connect.AssertEqual(t, fake.metadata("sub_clearedbad")["card_check"], "")
		connect.AssertEqual(t, fake.metadata("sub_clearedbad")["payment_sheet"], "attached")
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, cleared.networkId), true)

		// a sheet whose trial the reconcile credited before the hold, and whose
		// card fails: never refused, and its window stands
		prior := newStripeCardCheckDbNetwork(t, ctx, "cardcheckprior")
		addStripeCardCheckTrial(fake, prior, "priorbad", true, "fail")
		_, err = stripeHandleInvoicePaid(&StripeEventInvoiceObject{Id: "in_priorbad"}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		window := stripeCardCheckWindowEnd(t, ctx, prior.networkId, "in_priorbad")
		if window == nil {
			t.Fatalf("no window credited")
		}
		deliverStripeCardCheckEvent(t, ctx, "setup_intent.succeeded", stripeCardCheckSetupIntent(prior.customerId, "pm_priorbad"))
		connect.AssertEqual(t, fake.metadata("sub_priorbad")["card_check"], "")
		connect.AssertEqual(t, fake.defaultPaymentMethod("sub_priorbad"), "pm_priorbad")
		_, detached, _ = fake.writes()
		connect.AssertEqual(t, slices.Contains(detached, "pm_priorbad"), false)
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_priorbad"}, prior.clientSession)
		requireVerifyStatus(t, "a sheet credited before", result, err, VerifyPurchaseStatusAlreadyCredited)
		connect.AssertEqual(t, stripeCardCheckWindowEnd(t, ctx, prior.networkId, "in_priorbad").Equal(*window), true)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, prior.networkId), true)
		connect.AssertEqual(t, stripeCardCheckRefusals(stripeCardCheckFlowPaymentSheet), refusalsBefore+1)
	})
}

// A completed sheet whose attach every path missed (no webhook attached it,
// no report came) is attached by the sweep, its card checked and its trial
// credited through the webhooks' own finalization; a sheet whose trial was
// credited before is left as it is. A sheet with no card on its subscription
// and only its customer's default card from an earlier purchase is abandoned:
// canceled after a day, the customer's card left as it is, and kept before
// (the owner, 2026-11-16: "Cancel after a day"); one whose card lands on its
// subscription just before the cancel is attached instead.
func TestStripeTrialCardCheckSweepAttachesMissedSheets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setProCycleTestData(t, 10*model.Tib)
		fake := newStripeCardCheckFake(t)
		now := server.NowUtc()
		dayOld := now.Add(-stripeAbandonedPaymentSheetAge - time.Hour)
		pending := map[string]string{"plan": "yearly", "payment_sheet": "pending"}
		// a sheet with no card on its subscription; customerCard, when set, is
		// its customer's default card from an earlier purchase
		addCardlessSheet := func(network *stripeCardCheckDbNetwork, name string, created time.Time, customerCard string) {
			if customerCard != "" {
				fake.addCard(customerCard, network.customerId, "pass")
			}
			fake.addCustomer(network.customerId, customerCard)
			trialEnd := created.Add(StripeSubscriptionTrialDays * 24 * time.Hour)
			fake.addSubscription("sub_"+name, network.customerId, network.networkId, "trialing", pending, "", "in_"+name, created, trialEnd)
			fake.addInvoice("in_"+name, "sub_"+name, 0, created, trialEnd)
		}

		missed := newStripeCardCheckDbNetwork(t, ctx, "cardcheckmissed")
		addStripeCardCheckTrial(fake, missed, "missed", true, "pass")
		missedBad := newStripeCardCheckDbNetwork(t, ctx, "cardcheckmissedbad")
		addStripeCardCheckTrial(fake, missedBad, "missedbad", true, "fail")
		credited := newStripeCardCheckDbNetwork(t, ctx, "cardcheckmissedcredited")
		addStripeCardCheckTrial(fake, credited, "missedcredited", true, "fail")
		_, err := stripeHandleInvoicePaid(&StripeEventInvoiceObject{Id: "in_missedcredited"}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		window := stripeCardCheckWindowEnd(t, ctx, credited.networkId, "in_missedcredited")
		if window == nil {
			t.Fatalf("no window credited")
		}
		oldCustomer := newStripeCardCheckDbNetwork(t, ctx, "cardcheckoldcustomer")
		addCardlessSheet(oldCustomer, "oldcustomer", dayOld, "pm_oldcustomer")
		youngCustomer := newStripeCardCheckDbNetwork(t, ctx, "cardcheckyoungcustomer")
		addCardlessSheet(youngCustomer, "youngcustomer", now.Add(-time.Hour), "pm_youngcustomer")
		creditedCustomer := newStripeCardCheckDbNetwork(t, ctx, "cardcheckcreditedcustomer")
		addCardlessSheet(creditedCustomer, "creditedcustomer", dayOld, "pm_creditedcustomer")
		_, err = stripeHandleInvoicePaid(&StripeEventInvoiceObject{Id: "in_creditedcustomer"}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		creditedCustomerWindow := stripeCardCheckWindowEnd(t, ctx, creditedCustomer.networkId, "in_creditedcustomer")
		lastMoment := newStripeCardCheckDbNetwork(t, ctx, "cardchecklastmoment")
		addCardlessSheet(lastMoment, "lastmoment", dayOld, "")
		fake.addCard("pm_lastmoment", lastMoment.customerId, "pass")
		fake.afterSearch = func() {
			// the sheet's card lands on its subscription after the search found it
			// card-less, before the read that comes before a cancel
			if fake.subscriptions["sub_lastmoment"]["default_payment_method"] == nil {
				fake.subscriptions["sub_lastmoment"]["default_payment_method"] = "pm_lastmoment"
			}
		}
		// held: no trial yet
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, missed.networkId), false)

		sweep, err := stripeSweepAbandonedSheets(session.Testing_CreateClientSession(ctx, nil), now, defaultStripeAbandonedSheetSettings())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, sweep.AttachedCount, 2)
		connect.AssertEqual(t, sweep.RefusedCount, 1)
		connect.AssertEqual(t, sweep.CreditedCount, 2)
		connect.AssertEqual(t, sweep.CanceledCount, 1)

		// the customer's old card is no reason to keep a day-old sheet: canceled,
		// and the card is still the customer's default, never detached
		connect.AssertEqual(t, fake.status("sub_oldcustomer"), "canceled")
		ledgerRows, _ := stripeVerifyDbCounts(t, ctx, oldCustomer.networkId, "in_oldcustomer")
		connect.AssertEqual(t, ledgerRows, 0)
		fake.stateLock.Lock()
		oldCustomerDefault := fake.customers[oldCustomer.customerId]["invoice_settings"].(map[string]any)["default_payment_method"]
		oldCustomerCardOwner := fake.paymentMethods["pm_oldcustomer"]["customer"]
		fake.stateLock.Unlock()
		connect.AssertEqual(t, oldCustomerDefault, "pm_oldcustomer")
		connect.AssertEqual(t, oldCustomerCardOwner, oldCustomer.customerId)
		// under a day: kept
		connect.AssertEqual(t, fake.status("sub_youngcustomer"), "trialing")
		// credited before: never canceled, its window as it was
		connect.AssertEqual(t, fake.status("sub_creditedcustomer"), "trialing")
		connect.AssertEqual(t, stripeCardCheckWindowEnd(t, ctx, creditedCustomer.networkId, "in_creditedcustomer").Equal(*creditedCustomerWindow), true)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, creditedCustomer.networkId), true)
		// its card landed at the last moment: attached through the card check
		connect.AssertEqual(t, fake.status("sub_lastmoment"), "trialing")
		connect.AssertEqual(t, fake.metadata("sub_lastmoment")["payment_sheet"], "attached")
		ledgerRows, _ = stripeVerifyDbCounts(t, ctx, lastMoment.networkId, "in_lastmoment")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, lastMoment.networkId), true)

		connect.AssertEqual(t, fake.metadata("sub_missed")["payment_sheet"], "attached")
		ledgerRows, renewalRows := stripeVerifyDbCounts(t, ctx, missed.networkId, "in_missed")
		connect.AssertEqual(t, ledgerRows, 1)
		connect.AssertEqual(t, renewalRows, 1)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, missed.networkId), true)
		result, err := VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_missed"}, missed.clientSession)
		requireVerifyStatus(t, "attached by the sweep", result, err, VerifyPurchaseStatusAlreadyCredited)

		connect.AssertEqual(t, fake.metadata("sub_missedbad")["card_check"], "cvc_failed")
		connect.AssertEqual(t, fake.defaultPaymentMethod("sub_missedbad"), "")
		ledgerRows, _ = stripeVerifyDbCounts(t, ctx, missedBad.networkId, "in_missedbad")
		connect.AssertEqual(t, ledgerRows, 0)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, missedBad.networkId), false)
		result, err = VerifyStripeCheckout(&VerifyStripeCheckoutArgs{SubscriptionId: "sub_missedbad"}, missedBad.clientSession)
		requireVerifyCardRefusal(t, "refused by the sweep", result, err)

		// the credited sheet: no stamp, its card on it, its window as it was
		connect.AssertEqual(t, fake.metadata("sub_missedcredited")["card_check"], "")
		connect.AssertEqual(t, fake.metadata("sub_missedcredited")["payment_sheet"], "pending")
		connect.AssertEqual(t, fake.defaultPaymentMethod("sub_missedcredited"), "pm_missedcredited")
		connect.AssertEqual(t, stripeCardCheckWindowEnd(t, ctx, credited.networkId, "in_missedcredited").Equal(*window), true)
		_, detached, canceled := fake.writes()
		connect.AssertEqual(t, detached, []string{"pm_missedbad"})
		connect.AssertEqual(t, canceled, []string{"sub_oldcustomer"})
	})
}
