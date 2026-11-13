package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Root-cause tests for what a Stripe free trial is posted as. The yearly
// plan's trial starts with a $0 invoice, which was posted as a sale, "Pro
// (new, no charge)" at "0.00 USD". The trial's first charge is a cycle's
// invoice, posted as a "renewal". Each runs the real ledger gate
// (stripeCreditInvoicePaidInTx), then the worker's read back from a fake
// Stripe API.

func stripeSalesTrialText(headline string, item string, amount string, method string) string {
	return strings.Replace(salesNoticeText(item, "acme", amount, method), "*Sale:*", "*"+headline+":*", 1)
}

func TestStripeTrialStartPostsATrialWithNoCharge(t *testing.T) {
	notices := captureSalesNotices(t)
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	start := time.Date(2026, 11, 8, 4, 7, 27, 0, time.UTC)
	trialEnd := start.Add(14 * 24 * time.Hour)
	stripeSalesApi(t, map[string]string{
		"/v1/invoices/in_trial": fmt.Sprintf(
			`{"id":"in_trial","billing_reason":"subscription_create","amount_paid":0,"total":0,"currency":"usd","charge":null,"payment_intent":null,"period_start":%d,"period_end":%d,"lines":{"data":[{"period":{"start":%d,"end":%d},"price":{"recurring":{"interval":"year"}}}]}}`,
			start.Unix(), start.Unix(), start.Unix(), trialEnd.Unix(),
		),
	})

	// stripeResolveInvoiceCredit's window: the trial, then the free period's
	// grace
	invoice := &StripeEventInvoiceObject{Id: "in_trial", Total: 0, Currency: "usd", BillingReason: "subscription_create"}
	credit := &stripeInvoiceCredit{
		networkId:   networkId,
		startTime:   start,
		paidEndTime: trialEnd,
		endTime:     trialEnd.Add(SubscriptionGracePeriod),
	}
	credited, err := stripeCreditInvoicePaidInTx(tx, context.Background(), credit, invoice, 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, credited, true)

	got := notices()
	connect.AssertEqual(t, len(got), 1)
	want := stripeSalesTrialText("Trial", "Pro yearly (new)", "no charge (free trial until 2026-11-22)", "Stripe")
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), want)
	// the worker's read back agrees
	got[0].Enrich(context.Background(), got[0])
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), want)
}

func TestStripeTrialConversionIsTheFirstSale(t *testing.T) {
	notices := captureSalesNotices(t)
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	start := time.Date(2026, 11, 8, 4, 7, 27, 0, time.UTC)
	trialEnd := start.Add(14 * 24 * time.Hour)
	yearEnd := trialEnd.Add(365 * 24 * time.Hour)
	// a subscription invoice's own period is the one before the one it
	// bills: here the trial
	stripeSalesApi(t, map[string]string{
		"/v1/invoices/in_first": fmt.Sprintf(
			`{"id":"in_first","billing_reason":"subscription_cycle","amount_paid":3999,"total":3999,"currency":"usd","charge":"ch_first","period_start":%d,"period_end":%d,"lines":{"data":[{"period":{"start":%d,"end":%d},"price":{"recurring":{"interval":"year"}}}]}}`,
			start.Unix(), trialEnd.Unix(), trialEnd.Unix(), yearEnd.Unix(),
		),
		"/v1/charges/ch_first": `{"id":"ch_first","payment_method_details":{"type":"card"}}`,
	})

	invoice := &StripeEventInvoiceObject{
		Id:            "in_first",
		Total:         3999,
		Currency:      "usd",
		BillingReason: "subscription_cycle",
		PeriodStart:   int(start.Unix()),
		PeriodEnd:     int(trialEnd.Unix()),
	}
	credit := &stripeInvoiceCredit{
		networkId:   networkId,
		startTime:   trialEnd,
		paidEndTime: yearEnd,
		endTime:     yearEnd.Add(manualPaymentGracePeriod),
	}
	credited, err := stripeCreditInvoicePaidInTx(tx, context.Background(), credit, invoice, model.UsdToNanoCents(27.99))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, credited, true)

	got := notices()
	connect.AssertEqual(t, len(got), 1)
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), salesNoticeText("Pro yearly (new, after a free trial)", "acme", "39.99 USD", "Stripe"))
	got[0].Enrich(context.Background(), got[0])
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), salesNoticeText("Pro yearly (new, after a free trial)", "acme", "39.99 USD", "Stripe (card)"))

	// a year later, the cycle's invoice looks back on a paid year: a renewal
	renewal := &StripeEventInvoiceObject{
		Id:            "in_second",
		Total:         3999,
		Currency:      "usd",
		BillingReason: "subscription_cycle",
		PeriodStart:   int(trialEnd.Unix()),
		PeriodEnd:     int(yearEnd.Unix()),
	}
	notice := stripeInvoiceSalesNotice(networkId, renewal, yearEnd, yearEnd.Add(365*24*time.Hour))
	connect.AssertEqual(t, notice.Kind, "renewal")
}

// The reconciler credits a missed invoice by its id alone: the notice makes
// no trial verdict from the window, and the read back decides. A charged
// invoice for a short period is a sale.
func TestStripeReconcilerCreditLeavesTheTrialToTheReadBack(t *testing.T) {
	networkId := server.NewId()
	start := time.Date(2026, 11, 8, 4, 7, 27, 0, time.UTC)
	shortEnd := start.Add(9 * 24 * time.Hour)
	stripeSalesApi(t, map[string]string{
		"/v1/invoices/in_short": fmt.Sprintf(
			`{"id":"in_short","billing_reason":"subscription_update","amount_paid":1250,"total":1250,"currency":"usd","charge":"ch_short","lines":{"data":[{"period":{"start":%d,"end":%d},"price":{"recurring":{"interval":"year"}}}]}}`,
			start.Unix(), shortEnd.Unix(),
		),
		"/v1/charges/ch_short": `{"id":"ch_short","payment_method_details":{"type":"card"}}`,
		"/v1/invoices/in_missed_trial": fmt.Sprintf(
			`{"id":"in_missed_trial","billing_reason":"subscription_create","amount_paid":0,"total":0,"currency":"usd","lines":{"data":[{"period":{"start":%d,"end":%d},"price":{"recurring":{"interval":"year"}}}]}}`,
			start.Unix(), start.Add(14*24*time.Hour).Unix(),
		),
	})

	notice := stripeInvoiceSalesNotice(networkId, &StripeEventInvoiceObject{Id: "in_short"}, start, shortEnd)
	connect.AssertEqual(t, notice.Trial, false)
	notice.Enrich(context.Background(), notice)
	connect.AssertEqual(t, salesNoticeLines(notice, "acme"), salesNoticeText("Pro yearly (plan change)", "acme", "12.50 USD", "Stripe (card)"))

	notice = stripeInvoiceSalesNotice(networkId, &StripeEventInvoiceObject{Id: "in_missed_trial"}, start, start.Add(14*24*time.Hour))
	connect.AssertEqual(t, notice.Trial, false)
	notice.Enrich(context.Background(), notice)
	connect.AssertEqual(t, salesNoticeLines(notice, "acme"), stripeSalesTrialText("Trial", "Pro yearly (new)", "no charge (free trial until 2026-11-22)", "Stripe"))
}
