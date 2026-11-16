package controller

// Database-backed tests of the failed-payment follow-up's ledger and its
// edges, with the fake Stripe API of subscription_stripe_payment_failed_db_test.go
// and an injected clock (stripeDunningNow): the ledger is stamped with the
// clock's times, a payment after the notice withdraws it unsent, concurrent
// copies of one event owe one notice, the backfill's dry run writes nothing and
// its send shares the webhook's ledger, and the portal's payment method flow.
// Needs the test database and redis (server.DefaultTestEnv).

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Fixes the dunning's clock at `now` for the test.
func setStripeDunningTestClock(t testing.TB, now time.Time) {
	previousNow := stripeDunningNow
	stripeDunningNow = func() time.Time { return now }
	t.Cleanup(func() {
		stripeDunningNow = previousNow
	})
}

// The ledger takes its times from the dunning's clock: the first failure and
// the first notice at the first event, the final notice at the event that owes
// it, and the balance's past-due notice dates from the first failure.
func TestStripeDunningLedgerTakesTheInjectedClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "dunningclock", "dunning-clock@synthetic.example")

		firstTime := server.NowUtc().Truncate(time.Second)
		created := firstTime.Add(-time.Hour)
		next := created.Add(2 * 24 * time.Hour)
		env.setSubscription("sub_synthetic_clock", "cus_synthetic_clock", &networkId, "past_due")
		env.setInvoice("in_synthetic_clock", "sub_synthetic_clock", "cus_synthetic_clock", "open", 1, created, &next, 4000)

		setStripeDunningTestClock(t, firstTime)
		deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_clock")
		failure := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_clock")
		if failure == nil || failure.NetworkId != networkId || failure.CustomerId != "cus_synthetic_clock" || failure.SubscriptionId != "sub_synthetic_clock" {
			t.Fatalf("the first failure must be recorded for the network: %+v", failure)
		}
		if !failure.FirstFailTime.Equal(firstTime) || failure.FirstNoticeTime == nil || !failure.FirstNoticeTime.Equal(firstTime) || failure.FinalNoticeTime != nil {
			t.Fatalf("first failure at the injected clock: %+v", failure)
		}
		if failure.HostedInvoiceUrl != "https://invoice.stripe.com/i/acct_synthetic/in_synthetic_clock" {
			t.Fatalf("hosted invoice url %q", failure.HostedInvoiceUrl)
		}

		finalTime := firstTime.Add(9 * 24 * time.Hour)
		finalNext := created.Add(14 * 24 * time.Hour)
		env.setInvoice("in_synthetic_clock", "sub_synthetic_clock", "cus_synthetic_clock", "open", 8, created, &finalNext, 4000)
		setStripeDunningTestClock(t, finalTime)
		deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_clock")
		failure = model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_clock")
		if failure.AttemptCount != 8 || !failure.LastFailTime.Equal(finalTime) || !failure.FirstNoticeTime.Equal(firstTime) ||
			failure.FinalNoticeTime == nil || !failure.FinalNoticeTime.Equal(finalTime) {
			t.Fatalf("final notice at the injected clock: %+v", failure)
		}

		pastDue := paymentPastDueForNetwork(ctx, networkId, finalTime, defaultStripeDunningSettings())
		if pastDue == nil || !pastDue.FailTime.Equal(firstTime) || pastDue.UpdatePaymentUrl != StripeUpdatePaymentMethodUrl {
			t.Fatalf("past-due notice: %+v", pastDue)
		}
		// a failure no event resolved stops showing once it is old
		stale := finalTime.Add(defaultStripeDunningSettings().PastDueNoticeMaxAge + time.Second)
		if pastDue := paymentPastDueForNetwork(ctx, networkId, stale, defaultStripeDunningSettings()); pastDue != nil {
			t.Fatalf("a failure older than the notice's age must not show: %+v", pastDue)
		}
	})
}

// A renewal paid after its notice was owed, before the delivery sent it: the
// invoice.paid webhook resolves the failure and withdraws the notice, so the
// customer never gets a reminder for a paid invoice, and the past-due notice
// ends.
func TestStripeDunningPaidInvoiceWithdrawsTheUnsentNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "dunning-paid@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "dunningpaid", email)

		now := server.NowUtc().Truncate(time.Second)
		setStripeDunningTestClock(t, now)
		created := now.Add(-time.Hour)
		next := created.Add(2 * 24 * time.Hour)
		env.setSubscription("sub_synthetic_paid", "cus_synthetic_paid", &networkId, "past_due")
		env.setInvoice("in_synthetic_paid", "sub_synthetic_paid", "cus_synthetic_paid", "open", 1, created, &next, 4000)
		deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_paid")
		if message := model.GetAccountMessageByKey(ctx, stripePaymentFailedTestTemplateName, "in_synthetic_paid/first"); message == nil || message.SentTime != nil {
			t.Fatalf("the first notice must be owed and unsent: %+v", message)
		}

		// the customer pays on the hosted page; Stripe reports it paid
		env.setSubscription("sub_synthetic_paid", "cus_synthetic_paid", &networkId, "active")
		env.setInvoice("in_synthetic_paid", "sub_synthetic_paid", "cus_synthetic_paid", "paid", 2, created, nil, 4000)
		result, err := StripeWebhook(stripeWebhookEvent(t, "invoice.paid", map[string]any{
			"id":           "in_synthetic_paid",
			"object":       "invoice",
			"total":        4000,
			"customer":     "cus_synthetic_paid",
			"currency":     "usd",
			"period_start": created.Unix(),
			"period_end":   created.Add(365 * 24 * time.Hour).Unix(),
		}), clientSession)
		if err != nil || result == nil {
			t.Fatalf("invoice.paid: %+v %v", result, err)
		}

		sender := newOutboxTestSender()
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 0 {
			t.Fatalf("a notice for a paid invoice was sent: %d", len(notices))
		}
		message := model.GetAccountMessageByKey(ctx, stripePaymentFailedTestTemplateName, "in_synthetic_paid/first")
		if message == nil || message.AbandonTime == nil || !strings.HasPrefix(message.LastError, "withdrawn:") {
			t.Fatalf("the unsent notice must be withdrawn: %+v", message)
		}
		failure := model.GetStripeInvoicePaymentFailure(ctx, "in_synthetic_paid")
		if failure == nil || failure.ResolveTime == nil || failure.ResolveReason != model.StripeInvoicePaymentFailureResolvePaid || failure.HostedInvoiceUrl != "" {
			t.Fatalf("the failure must be resolved as paid, its link cleared: %+v", failure)
		}
		if pastDue := paymentPastDueForNetwork(ctx, networkId, now, defaultStripeDunningSettings()); pastDue != nil {
			t.Fatalf("a paid invoice must end the past-due notice: %+v", pastDue)
		}
		// the paid renewal credited the network as before
		if renewal := model.GetLastSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter, model.SubscriptionMarketStripe); renewal == nil || renewal.TransactionId != "in_synthetic_paid" {
			t.Fatalf("the paid invoice must credit the network: %+v", renewal)
		}

		// a late copy of the failure after the payment owes nothing
		deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_paid")
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 0 {
			t.Fatalf("a late failure event after the payment sent %d notices", len(notices))
		}
	})
}

// Concurrent copies of one failure event decide one after the other on the
// invoice's locked ledger row: one notice is owed and one email is sent.
func TestStripeDunningConcurrentReplaysOweOneNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "dunning-concurrent@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "dunningconcurrent", email)

		now := server.NowUtc().Truncate(time.Second)
		setStripeDunningTestClock(t, now)
		created := now.Add(-time.Hour)
		next := created.Add(2 * 24 * time.Hour)
		env.setSubscription("sub_synthetic_concurrent", "cus_synthetic_concurrent", &networkId, "past_due")
		env.setInvoice("in_synthetic_concurrent", "sub_synthetic_concurrent", "cus_synthetic_concurrent", "open", 1, created, &next, 4000)

		const copyCount = 8
		var wait sync.WaitGroup
		errs := make(chan error, copyCount)
		for range copyCount {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, err := stripeHandleInvoicePaymentFailed([]byte(`{"id":"in_synthetic_concurrent","billing_reason":"subscription_cycle"}`), clientSession)
				errs <- err
			}()
		}
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		sender := newOutboxTestSender()
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 1 {
			t.Fatalf("%d concurrent copies sent %d notices, want 1", copyCount, len(notices))
		}
	})
}

// The backfill's dry run reads Stripe and the database and writes nothing; its
// send owes the notices through the webhook's processing, so a later webhook
// for the same invoice owes nothing more.
func TestStripeDunningBackfillDryRunWritesNothingAndSendSharesTheLedger(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "dunning-backfill@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "dunningbackfill", email)
		walletNetworkId, _ := newStripePaymentFailedTestNetwork(t, ctx, "dunningbackfillwallet", "")

		now := server.NowUtc().Truncate(time.Second)
		setStripeDunningTestClock(t, now)
		day := 24 * time.Hour
		created := now.Add(-time.Hour)
		next := created.Add(2 * day)
		lastNext := created.Add(13 * day)
		env.setSubscription("sub_synthetic_backfill", "cus_synthetic_backfill", &networkId, "past_due")
		env.setInvoice("in_synthetic_backfill", "sub_synthetic_backfill", "cus_synthetic_backfill", "open", 1, created, &next, 4000)
		env.setSubscription("sub_synthetic_backfill_wallet", "cus_synthetic_backfill_wallet", &walletNetworkId, "past_due")
		env.setInvoice("in_synthetic_backfill_wallet", "sub_synthetic_backfill_wallet", "cus_synthetic_backfill_wallet", "open", 8, created, &lastNext, 500)
		// a past-due subscription whose subscription and checkout name no network
		env.setSubscription("sub_synthetic_backfill_orphan", "cus_synthetic_backfill_orphan", nil, "past_due")
		env.setInvoice("in_synthetic_backfill_orphan", "sub_synthetic_backfill_orphan", "cus_synthetic_backfill_orphan", "open", 2, created, &next, 4000)
		for _, subscriptionId := range []string{"sub_synthetic_backfill", "sub_synthetic_backfill_wallet", "sub_synthetic_backfill_orphan"} {
			invoiceId := strings.Replace(subscriptionId, "sub_", "in_", 1)
			env.setLatestInvoice(subscriptionId, invoiceId)
		}

		rowsBySubscription := func(rows []*StripeDunningReportRow) map[string]*StripeDunningReportRow {
			bySubscription := map[string]*StripeDunningReportRow{}
			for _, row := range rows {
				bySubscription[row.SubscriptionId] = row
			}
			return bySubscription
		}

		rows, err := StripeDunningReport(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		dryRun := rowsBySubscription(rows)
		if row := dryRun["sub_synthetic_backfill"]; row == nil || row.NetworkId == nil || *row.NetworkId != networkId || !row.HasEmail ||
			row.Notice != string(stripePaymentFailedNoticeFirst) || row.AttemptCount != 1 || row.Amount() != "$40.00" {
			t.Fatalf("dry run, emailed account: %+v", row)
		}
		if row := dryRun["sub_synthetic_backfill_wallet"]; row == nil || row.NetworkId == nil || *row.NetworkId != walletNetworkId || row.HasEmail ||
			row.Notice != string(stripePaymentFailedNoticeFinal) {
			t.Fatalf("dry run, account without email at its last retry: %+v", row)
		}
		if row := dryRun["sub_synthetic_backfill_orphan"]; row == nil || row.NetworkId != nil || row.Notice != "" || row.Skip == "" {
			t.Fatalf("dry run, no network: %+v", row)
		}
		for _, invoiceId := range []string{"in_synthetic_backfill", "in_synthetic_backfill_wallet", "in_synthetic_backfill_orphan"} {
			if failure := model.GetStripeInvoicePaymentFailure(ctx, invoiceId); failure != nil {
				t.Fatalf("the dry run recorded %s: %+v", invoiceId, failure)
			}
		}
		if message := model.GetAccountMessageByKey(ctx, stripePaymentFailedTestTemplateName, "in_synthetic_backfill/first"); message != nil {
			t.Fatalf("the dry run owed a notice: %+v", message)
		}

		rows, err = StripeDunningReport(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		if row := rowsBySubscription(rows)["sub_synthetic_backfill"]; row == nil || row.Notice != string(stripePaymentFailedNoticeFirst) || !row.HasEmail {
			t.Fatalf("send: %+v", row)
		}
		sender := newOutboxTestSender()
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 1 {
			t.Fatalf("the send must email the account once, got %d", len(notices))
		}

		// the webhook for the same failure, and a second send, owe nothing more
		deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_backfill")
		if _, err := StripeDunningReport(ctx, true); err != nil {
			t.Fatal(err)
		}
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 1 {
			t.Fatalf("the webhook after the send emailed again: %d notices", len(notices))
		}
	})
}

// The payment method flow opens the portal on updating the payment method and
// returns to Manage Subscription; a portal configuration that refuses the flow
// still opens the portal's home.
func TestStripeCustomerPortalPaymentMethodUpdateFlow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)
		setStripeDunningTestClock(t, now)
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "portalflow", "portal-flow@synthetic.example")

		// the customer of the failing renewal, which has no stripe_customer row
		created := now.Add(-time.Hour)
		next := created.Add(2 * 24 * time.Hour)
		env.setSubscription("sub_synthetic_flow", "cus_synthetic_flow", &networkId, "past_due")
		env.setInvoice("in_synthetic_flow", "sub_synthetic_flow", "cus_synthetic_flow", "open", 1, created, &next, 4000)
		deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_flow")

		portal, err := StripeCreateCustomerPortal(&StripeCreateCustomerPortalArgs{Flow: StripeCustomerPortalFlowPaymentMethodUpdate}, clientSession)
		if err != nil || portal.Error != nil || portal.Url != "https://billing.stripe.example/p/session/cus_synthetic_flow" {
			t.Fatalf("payment method flow: %+v %v", portal, err)
		}
		forms := env.postedPortalForms()
		form := forms[len(forms)-1]
		if form.Get("customer") != "cus_synthetic_flow" ||
			form.Get("flow_data[type]") != "payment_method_update" ||
			form.Get("flow_data[after_completion][type]") != "redirect" ||
			form.Get("flow_data[after_completion][redirect][return_url]") != StripeUpdatePaymentMethodUrl ||
			form.Get("return_url") != StripeUpdatePaymentMethodUrl {
			t.Fatalf("payment method flow form: %v", form)
		}

		env.setRefusePortalFlow(true)
		portal, err = StripeCreateCustomerPortal(&StripeCreateCustomerPortalArgs{Flow: StripeCustomerPortalFlowPaymentMethodUpdate}, clientSession)
		if err != nil || portal.Error != nil || portal.Url == "" {
			t.Fatalf("a refused flow must fall back to the portal home: %+v %v", portal, err)
		}
		forms = env.postedPortalForms()
		if fallback := forms[len(forms)-1]; fallback.Get("flow_data[type]") != "" || fallback.Get("customer") != "cus_synthetic_flow" {
			t.Fatalf("fallback form: %v", fallback)
		}

		// the details name the customer and the past due renewal's payment page
		clearSubscriptionDetailsCached(ctx, networkId)
		details, err := SubscriptionDetails(clientSession)
		if err != nil || !details.HasStripeCustomer {
			t.Fatalf("details must offer the portal for the checkout customer: %+v %v", details, err)
		}
	})
}
