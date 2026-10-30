package controller

// Hermetic tests of the failed-payment follow-up's decisions
// (subscription_stripe_dunning.go): which notice a failed renewal owes, given
// the invoice Stripe reports and the invoice's ledger row, with no database and
// no clock. The webhook paths through the database are in
// subscription_stripe_dunning_db_test.go.

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// A synthetic open renewal invoice made at `created`, after `attemptCount`
// failed attempts, with Stripe's next attempt at `next` (nil: no more).
func stripeDunningTestInvoice(created time.Time, attemptCount int, next *time.Time) *stripeDunningInvoice {
	invoice := &stripeDunningInvoice{
		Id:            "in_synthetic_dunning",
		Status:        "open",
		BillingReason: stripeBillingReasonSubscriptionCycle,
		AttemptCount:  attemptCount,
		AmountDue:     4000,
		Currency:      "usd",
		Created:       created.Unix(),
	}
	if next != nil {
		nextUnix := next.Unix()
		invoice.NextPaymentAttempt = &nextUnix
	}
	return invoice
}

func stripeDunningTestTime(t time.Time) *time.Time {
	return &t
}

// A renewal that fails nine times owes exactly two notices: the first at the
// first failure, and the final once the next attempt falls in the last days of
// the retry window. Every other failure, and every failure after the final
// notice, owes nothing.
func TestStripePaymentFailedNoticeDueOwesFirstThenFinalThenNothing(t *testing.T) {
	settings := defaultStripeDunningSettings()
	created := time.Date(2026, 10, 12, 16, 38, 0, 0, time.UTC)
	day := 24 * time.Hour
	// Smart Retries on the account: 8 retries within 2 weeks
	nextAttemptOffsets := []time.Duration{
		2 * day, 4 * day, 6 * day, 8 * day, 9 * day, 11 * day, 13 * day, 14 * day,
	}

	recorded := &model.StripeInvoicePaymentFailure{}
	notices := []stripePaymentFailedNotice{}
	for attemptCount := 1; attemptCount <= 9; attemptCount += 1 {
		var next *time.Time
		if attemptCount <= len(nextAttemptOffsets) {
			next = stripeDunningTestTime(created.Add(nextAttemptOffsets[attemptCount-1]))
		}
		notice := stripePaymentFailedNoticeDue(stripeDunningTestInvoice(created, attemptCount, next), recorded, settings)
		switch notice {
		case stripePaymentFailedNoticeFirst:
			recorded.FirstNoticeTime = stripeDunningTestTime(created)
		case stripePaymentFailedNoticeFinal:
			recorded.FinalNoticeTime = stripeDunningTestTime(created)
			if recorded.FirstNoticeTime == nil {
				recorded.FirstNoticeTime = recorded.FinalNoticeTime
			}
		}
		notices = append(notices, notice)
	}

	want := []stripePaymentFailedNotice{
		stripePaymentFailedNoticeFirst,
		stripePaymentFailedNoticeNone,
		stripePaymentFailedNoticeNone,
		stripePaymentFailedNoticeNone,
		stripePaymentFailedNoticeNone,
		// the sixth failure: the next attempt, at day 11, is within 4 days of
		// the window's end
		stripePaymentFailedNoticeFinal,
		stripePaymentFailedNoticeNone,
		stripePaymentFailedNoticeNone,
		stripePaymentFailedNoticeNone,
	}
	for i := range want {
		if notices[i] != want[i] {
			t.Fatalf("attempt %d: notice %q, want %q (all: %q)", i+1, notices[i], want[i], notices)
		}
	}
}

// The final notice is due when the next attempt is the last by count, when it
// falls in the last days of the retry window, or when Stripe will not retry
// again; not before.
func TestStripeDunningFinalDue(t *testing.T) {
	settings := defaultStripeDunningSettings()
	created := time.Date(2026, 10, 16, 18, 23, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name         string
		attemptCount int
		next         *time.Time
		final        bool
	}{
		{"first failure, early retry", 1, stripeDunningTestTime(created.Add(2 * day)), false},
		{"mid window", 4, stripeDunningTestTime(created.Add(9*day + 23*time.Hour)), false},
		{"next attempt within the lead of the window end", 4, stripeDunningTestTime(created.Add(10 * day)), true},
		{"the next attempt is the ninth and last", 8, stripeDunningTestTime(created.Add(5 * day)), true},
		{"no next attempt", 3, nil, true},
		{"no next attempt on the first failure", 1, nil, true},
	}
	for _, c := range cases {
		final := stripeDunningFinalDue(stripeDunningTestInvoice(created, c.attemptCount, c.next), settings)
		if final != c.final {
			t.Errorf("%s: final %t, want %t", c.name, final, c.final)
		}
	}
}

// A first decided failure that is already the last owes the final notice
// alone; a late copy of an earlier failure then owes nothing.
func TestStripePaymentFailedNoticeDueFinalStandsForTheFirst(t *testing.T) {
	settings := defaultStripeDunningSettings()
	created := time.Date(2026, 10, 13, 20, 46, 0, 0, time.UTC)

	notice := stripePaymentFailedNoticeDue(stripeDunningTestInvoice(created, 8, stripeDunningTestTime(created.Add(14*24*time.Hour))), nil, settings)
	if notice != stripePaymentFailedNoticeFinal {
		t.Fatalf("the first decided failure at the last retry: %q, want final", notice)
	}
	finalTime := created.Add(13 * 24 * time.Hour)
	// SetStripeInvoicePaymentFailureFinalNoticeInTx sets both times
	recorded := &model.StripeInvoicePaymentFailure{FirstNoticeTime: &finalTime, FinalNoticeTime: &finalTime}
	late := stripePaymentFailedNoticeDue(stripeDunningTestInvoice(created, 1, stripeDunningTestTime(created.Add(2*24*time.Hour))), recorded, settings)
	if late != stripePaymentFailedNoticeNone {
		t.Fatalf("a late copy of the first failure after the final notice: %q, want none", late)
	}
}

// A resolved failure owes nothing, whatever Stripe's copy says.
func TestStripePaymentFailedNoticeDueNothingAfterResolve(t *testing.T) {
	settings := defaultStripeDunningSettings()
	created := time.Date(2026, 10, 18, 9, 40, 0, 0, time.UTC)
	resolveTime := created.Add(3 * 24 * time.Hour)
	recorded := &model.StripeInvoicePaymentFailure{
		ResolveTime:   &resolveTime,
		ResolveReason: model.StripeInvoicePaymentFailureResolvePaid,
	}
	for _, next := range []*time.Time{nil, stripeDunningTestTime(created.Add(24 * time.Hour))} {
		if notice := stripePaymentFailedNoticeDue(stripeDunningTestInvoice(created, 1, next), recorded, settings); notice != stripePaymentFailedNoticeNone {
			t.Fatalf("resolved: %q, want none", notice)
		}
	}
}

// Only an open renewal invoice whose subscription Stripe is still retrying is
// a failing renewal: a paid, voided or uncollectible invoice, a first charge, a
// one-time invoice, and a subscription that ended owe nothing.
func TestStripeDunningFailingReason(t *testing.T) {
	created := time.Date(2026, 10, 20, 12, 38, 0, 0, time.UTC)
	next := stripeDunningTestTime(created.Add(2 * 24 * time.Hour))
	retrying := &stripeDunningSubscription{Id: "sub_synthetic", Status: "past_due"}
	cases := []struct {
		name    string
		change  func(invoice *stripeDunningInvoice, sub *stripeDunningSubscription)
		failing bool
	}{
		{"past due renewal", func(*stripeDunningInvoice, *stripeDunningSubscription) {}, true},
		{"unpaid renewal", func(_ *stripeDunningInvoice, sub *stripeDunningSubscription) { sub.Status = "unpaid" }, true},
		{"paid", func(invoice *stripeDunningInvoice, _ *stripeDunningSubscription) { invoice.Status = "paid" }, false},
		{"voided", func(invoice *stripeDunningInvoice, _ *stripeDunningSubscription) { invoice.Status = "void" }, false},
		{"uncollectible", func(invoice *stripeDunningInvoice, _ *stripeDunningSubscription) { invoice.Status = "uncollectible" }, false},
		{"first charge", func(invoice *stripeDunningInvoice, _ *stripeDunningSubscription) {
			invoice.BillingReason = "subscription_create"
		}, false},
		{"one-time invoice", func(invoice *stripeDunningInvoice, _ *stripeDunningSubscription) { invoice.BillingReason = "manual" }, false},
		{"subscription canceled", func(_ *stripeDunningInvoice, sub *stripeDunningSubscription) { sub.Status = "canceled" }, false},
		{"subscription incomplete", func(_ *stripeDunningInvoice, sub *stripeDunningSubscription) { sub.Status = "incomplete" }, false},
		{"no attempt yet", func(invoice *stripeDunningInvoice, _ *stripeDunningSubscription) { invoice.AttemptCount = 0 }, false},
	}
	for _, c := range cases {
		invoice := stripeDunningTestInvoice(created, 1, next)
		sub := *retrying
		c.change(invoice, &sub)
		reason := stripeDunningFailingReason(invoice, &sub)
		if (reason == "") != c.failing {
			t.Errorf("%s: reason %q, failing want %t", c.name, reason, c.failing)
		}
	}
	if reason := stripeDunningFailingReason(stripeDunningTestInvoice(created, 1, next), nil); reason == "" {
		t.Errorf("an invoice with no subscription is not a failing renewal")
	}
}

// The resolve reasons of the invoice states that end a failure.
func TestStripeDunningResolveReason(t *testing.T) {
	for status, want := range map[string]string{
		"paid":          model.StripeInvoicePaymentFailureResolvePaid,
		"void":          model.StripeInvoicePaymentFailureResolveVoided,
		"uncollectible": model.StripeInvoicePaymentFailureResolveUncollectible,
		"open":          "",
		"draft":         "",
	} {
		if got := stripeDunningResolveReason(status); got != want {
			t.Errorf("status %s: %q, want %q", status, got, want)
		}
	}
}

// Only Stripe's hosted invoice pages are linked from a notice.
func TestStripeHostedInvoiceUrlAllowed(t *testing.T) {
	for url, allowed := range map[string]bool{
		"https://invoice.stripe.com/i/acct_synthetic/live_synthetic?s=ap": true,
		"http://invoice.stripe.com/i/acct_synthetic/live_synthetic":       false,
		"https://invoice.stripe.com.attacker.example/i/x":                 false,
		"https://pay.attacker.example/i/x":                                false,
		"":                                                                false,
	} {
		if got := stripeHostedInvoiceUrlAllowed(url); (got != "") != allowed || (allowed && got != url) {
			t.Errorf("%q: %q, allowed %t", url, got, allowed)
		}
	}
}

func TestStripeFormatAmount(t *testing.T) {
	for _, c := range []struct {
		amount   int64
		currency string
		want     string
	}{
		{4000, "usd", "$40.00"},
		{400, "USD", "$4.00"},
		{5, "usd", "$0.05"},
		{1999, "eur", "19.99 EUR"},
		{4000, "jpy", "4000 JPY"},
	} {
		if got := stripeFormatAmount(c.amount, c.currency); got != c.want {
			t.Errorf("%d %s: %q, want %q", c.amount, c.currency, got, c.want)
		}
	}
}

// The notices name the amount and the next attempt, link both the invoice's
// Stripe page and Manage Subscription in the html and the text part, and say
// which notice they are in the subject. Without a hosted page the email offers
// the payment method update alone.
func TestSubscriptionPaymentFailedTemplateRendersBothLinks(t *testing.T) {
	const hostedInvoiceUrl = "https://invoice.stripe.com/i/acct_synthetic/live_synthetic?s=ap"
	nextAttempt := time.Date(2026, 10, 30, 19, 24, 0, 0, time.UTC)

	first := &SubscriptionPaymentFailedTemplate{
		HostedInvoiceUrl: hostedInvoiceUrl,
		AmountDue:        4000,
		Currency:         "usd",
		NextAttemptTime:  &nextAttempt,
	}
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(first)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Your URnetwork payment didn't go through" {
		t.Errorf("first subject %q", subject)
	}
	normalText := strings.Join(strings.Fields(bodyText), " ")
	for _, want := range []string{"$40.00", "October 30, 2026", "update your payment method"} {
		if !strings.Contains(bodyHtml, want) || !strings.Contains(strings.ToLower(normalText), strings.ToLower(want)) {
			t.Errorf("first notice missing %q", want)
		}
	}
	for _, link := range []string{hostedInvoiceUrl, StripeUpdatePaymentMethodUrl} {
		if !strings.Contains(bodyText, link) {
			t.Errorf("first notice text part missing link %s", link)
		}
	}
	if !strings.Contains(bodyHtml, `href="https://invoice.stripe.com/i/acct_synthetic/live_synthetic?s=ap"`) ||
		!strings.Contains(bodyHtml, `href="`+StripeUpdatePaymentMethodUrl+`"`) {
		t.Errorf("first notice html must link the invoice and Manage Subscription")
	}
	if strings.Contains(bodyHtml, "last reminder") {
		t.Errorf("the first notice must not read as the last")
	}

	final := &SubscriptionPaymentFailedTemplate{
		Final:            true,
		HostedInvoiceUrl: hostedInvoiceUrl,
		AmountDue:        500,
		Currency:         "usd",
	}
	subject, bodyHtml, bodyText, err = RenderEmailTemplate(final)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Final notice: your URnetwork payment didn't go through" {
		t.Errorf("final subject %q", subject)
	}
	normalText = strings.Join(strings.Fields(bodyText), " ")
	for _, want := range []string{"$5.00", "last reminder", "subscription will end"} {
		if !strings.Contains(bodyHtml, want) || !strings.Contains(normalText, want) {
			t.Errorf("final notice missing %q", want)
		}
	}
	if strings.Contains(bodyText, "next automatic attempt") {
		t.Errorf("a final notice with no next attempt must not name one")
	}

	noHostedPage := &SubscriptionPaymentFailedTemplate{AmountDue: 4000, Currency: "usd"}
	_, bodyHtml, bodyText, err = RenderEmailTemplate(noHostedPage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodyHtml, "invoice.stripe.com") || strings.Contains(bodyText, "Pay the invoice:") {
		t.Errorf("without a hosted page the email must not offer to pay the invoice")
	}
	if !strings.Contains(bodyHtml, `href="`+StripeUpdatePaymentMethodUrl+`"`) || !strings.Contains(bodyText, StripeUpdatePaymentMethodUrl) {
		t.Errorf("without a hosted page the email must still link Manage Subscription")
	}

	sms, err := RenderSmsTemplate(first)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sms, "$40.00") || !strings.Contains(sms, StripeUpdatePaymentMethodUrl) || 160 < len(sms) {
		t.Errorf("sms must name the amount and the update link in one segment: %d bytes %q", len(sms), sms)
	}
}
