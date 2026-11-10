package controller

// What a Team invoice bills, read from its lines (QUOTA-M6-02). The events run against a fake
// Stripe in team_stripe_events_db_test.go.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

func testSetStripeTeamPriceIds() func() {
	previous := stripeTeamPrices
	stripeTeamPrices = func() StripeTeamPrices {
		return StripeTeamPrices{
			Base:            "price_base",
			Seats:           "price_seats",
			TransferOverage: "price_transfer",
			StorageOverage:  "price_storage",
		}
	}
	return func() { stripeTeamPrices = previous }
}

func testParseStripeInvoiceExpanded(t testing.TB, invoiceJson string) *StripeInvoiceExpanded {
	t.Helper()
	fullInvoice := &StripeInvoiceExpanded{}
	if err := json.Unmarshal([]byte(invoiceJson), fullInvoice); err != nil {
		t.Fatal(err)
	}
	return fullInvoice
}

// A Team invoice is told by its subscription's mark or by a line at a Team price, in both shapes
// an invoice line names its price; a Pro invoice is neither.
func TestStripeInvoiceTeamMark(t *testing.T) {
	defer testSetStripeTeamPriceIds()()

	marked := testParseStripeInvoiceExpanded(t, `{
		"subscription": {"id": "sub_1", "metadata": {"network_id": "n", "team": "1"}},
		"lines": {"data": [{"type": "subscription", "price": {"id": "price_other"}}]}
	}`)
	connect.AssertEqual(t, marked.team(), true)

	priced := testParseStripeInvoiceExpanded(t, `{
		"subscription": {"id": "sub_1", "metadata": {"network_id": "n"}},
		"lines": {"data": [{"type": "subscription", "price": {"id": "price_seats"}}]}
	}`)
	connect.AssertEqual(t, priced.team(), true)

	pricedNewShape := testParseStripeInvoiceExpanded(t, `{
		"subscription": {"id": "sub_1", "metadata": {}},
		"lines": {"data": [{"type": "subscription", "pricing": {"price_details": {"price": "price_base"}}}]}
	}`)
	connect.AssertEqual(t, pricedNewShape.team(), true)

	pro := testParseStripeInvoiceExpanded(t, `{
		"subscription": {"id": "sub_1", "metadata": {"network_id": "n", "plan": "monthly"}},
		"lines": {"data": [{"type": "subscription", "price": {"id": "price_pro_monthly"}}, {"type": "subscription"}]}
	}`)
	connect.AssertEqual(t, pro.team(), false)

	// an env with no team prices matches no line by an empty id
	previous := stripeTeamPrices
	stripeTeamPrices = func() StripeTeamPrices { return StripeTeamPrices{} }
	connect.AssertEqual(t, pro.team(), false)
	stripeTeamPrices = previous
}

// The period a Team invoice buys is its base item's, though the first line is last period's
// metered item; overage lines are summed apart; an invoice with no base line buys no period, and
// one whose lines were not all read is an error rather than a guess.
func TestStripeTeamInvoiceOf(t *testing.T) {
	defer testSetStripeTeamPriceIds()()
	lastStart := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 11, 0, 0, 0, 0, time.UTC)
	period := func(start time.Time, end time.Time) *StripeLineItemPeriod {
		return &StripeLineItemPeriod{Start: start.Unix(), End: end.Unix()}
	}
	line := func(priceId string, amount int64, period *StripeLineItemPeriod) *StripeLineItem {
		return &StripeLineItem{Type: "subscription", Price: &StripeLineItemProduct{Id: priceId}, Amount: amount, Period: period}
	}

	cycle := &StripeInvoiceExpanded{Lines: &StripeLineItems{Data: []*StripeLineItem{
		// arrears of the period before come first
		line("price_seats", 4000, period(lastStart, start)),
		line("price_transfer", 293, period(lastStart, start)),
		line("price_storage", 66, period(lastStart, start)),
		nil,
		line("price_base", 10000, period(start, end)),
	}}}
	teamInvoice, err := stripeTeamInvoiceOf("in_cycle", cycle)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, teamInvoice.hasBase, true)
	connect.AssertEqual(t, teamInvoice.periodStart, start)
	connect.AssertEqual(t, teamInvoice.periodEnd, end)
	connect.AssertEqual(t, teamInvoice.overageAmount, int64(359))
	// the renewal's revenue leaves the overage out: (14359 - 359) cents less fees
	connect.AssertEqual(
		t,
		stripeTeamInvoiceNetRevenue(&StripeEventInvoiceObject{Id: "in_cycle", Total: 14359}, teamInvoice),
		model.UsdToNanoCents(0.7*140),
	)
	connect.AssertEqual(t, stripeTeamInvoiceNetRevenue(&StripeEventInvoiceObject{Id: "in_cycle", Total: 100}, teamInvoice), model.NanoCents(0))

	// a proration splits the base: the newest period is the one bought
	prorated := &StripeInvoiceExpanded{Lines: &StripeLineItems{Data: []*StripeLineItem{
		line("price_base", -500, period(lastStart, start)),
		line("price_base", 10000, period(start, end)),
	}}}
	teamInvoice, err = stripeTeamInvoiceOf("in_prorated", prorated)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, teamInvoice.periodStart, start)
	connect.AssertEqual(t, teamInvoice.periodEnd, end)

	// the final invoice of metered usage: no period
	final := &StripeInvoiceExpanded{Lines: &StripeLineItems{Data: []*StripeLineItem{
		line("price_seats", 4000, period(lastStart, start)),
		line("price_transfer", 293, period(lastStart, start)),
	}}}
	teamInvoice, err = stripeTeamInvoiceOf("in_final", final)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, teamInvoice.hasBase, false)
	connect.AssertEqual(t, teamInvoice.overageAmount, int64(293))

	// more lines than the page holds and no base among them
	final.Lines.HasMore = true
	if _, err := stripeTeamInvoiceOf("in_paged", final); err == nil {
		t.Fatal("an invoice with unread lines was read as one with no base line")
	}
	if _, err := stripeTeamInvoiceOf("in_no_lines", &StripeInvoiceExpanded{}); err == nil {
		t.Fatal("an invoice with no lines was read")
	}
}
