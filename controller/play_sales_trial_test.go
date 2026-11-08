package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Root-cause tests for what a credited Play window is posted as. Every window
// the credit gate added was posted as a sale: the 14-day free trial of the
// yearly plan as a sale at the yearly list price (penguin1234, 2026-11-08),
// and the day Play moves the expiry out before nearly every renewal, while
// the renewal's charge is pending, as another sale of the same order. Each
// runs through the real credit gate (playCreditSubscriptionInTx) on
// salesLedgerTx.

const playSalesTrialToken = "token-penguin"

type playSalesTrialFixture struct {
	t             *testing.T
	notices       func() []*model.SalesPaymentNotice
	tx            *salesLedgerTx
	clientSession *session.ClientSession
	args          *PlaySubscriptionRenewalArgs
	sku           *Sku
	start         time.Time
}

func newPlaySalesTrialFixture(t *testing.T) *playSalesTrialFixture {
	networkId := server.NewId()
	return &playSalesTrialFixture{
		t:             t,
		notices:       captureSalesNotices(t),
		tx:            newSalesLedgerTx(networkId),
		clientSession: session.Testing_CreateClientSession(context.Background(), nil),
		args: &PlaySubscriptionRenewalArgs{
			NetworkId:      networkId,
			PackageName:    "network.ur.test",
			SubscriptionId: "supporter",
			PurchaseToken:  playSalesTrialToken,
		},
		sku: &Sku{Supporter: true, PriceAmountUsd: 5, FeeFraction: 0.3},
		// the credit refuses a window that already ended, so the purchase
		// starts an hour ago
		start: server.NowUtc().Add(-time.Hour).Truncate(time.Second),
	}
}

// credit runs the gate for sub through expiry and reports whether it credited.
func (self *playSalesTrialFixture) credit(sub *PlaySubscription, expiry time.Time) bool {
	self.t.Helper()
	sub.LineItems[0].ExpiryTime = expiry.UTC().Format(time.RFC3339)
	credited, err := playCreditSubscriptionInTx(self.tx, self.clientSession, self.args, sub, self.sku, self.start, expiry)
	connect.AssertEqual(self.t, err, nil)
	return credited
}

func (self *playSalesTrialFixture) texts() []string {
	texts := []string{}
	for _, notice := range self.notices() {
		texts = append(texts, salesNoticeLines(notice, "penguin1234"))
	}
	return texts
}

func playSalesText(headline string, item string, amount string) string {
	return "*" + headline + ":* " + item + "\n*Network:* penguin1234\n*Amount:* " + amount + "\n*Payment method:* Google Play"
}

// Play's yearly trial is posted as a trial with no charge, on the yearly
// plan.
func TestPlayTrialStartPostsATrialWithNoCharge(t *testing.T) {
	fixture := newPlaySalesTrialFixture(t)
	// Play reports the trial's expiry a moment short of 14 days
	trialEnd := fixture.start.Add(14*24*time.Hour - 2*time.Second)
	sub := playPenguinSubscription(fixture.start, trialEnd)

	connect.AssertEqual(t, fixture.credit(sub, trialEnd), true)
	connect.AssertEqual(t, fixture.texts(), []string{
		playSalesText("Trial", "Pro yearly (new)", "no charge (free trial until "+trialEnd.Format("2006-01-02")+")"),
	})
}

// The day Play moves the expiry out while a charge is pending is credited
// (the customer is entitled through it) but is not a sale: no new order, no
// notice. The renewal's charge that follows is one sale.
func TestPlayWindowMovedWithoutAChargePostsNothing(t *testing.T) {
	fixture := newPlaySalesTrialFixture(t)
	trialEnd := fixture.start.Add(14*24*time.Hour - 2*time.Second)
	sub := playPenguinSubscription(fixture.start, trialEnd)
	connect.AssertEqual(t, fixture.credit(sub, trialEnd), true)

	// a day and a second later, the same order: the trial's conversion charge
	// is pending, twice
	connect.AssertEqual(t, fixture.credit(sub, trialEnd.Add(24*time.Hour+time.Second)), true)
	connect.AssertEqual(t, fixture.credit(sub, trialEnd.Add(48*time.Hour+2*time.Second)), true)
	connect.AssertEqual(t, len(fixture.notices()), 1)

	// a monthly purchase: its first month, Play's extra day, then the renewal
	monthly := newPlaySalesTrialFixture(t)
	monthEnd := monthly.start.Add(30 * 24 * time.Hour)
	monthSub := playPenguinSubscription(monthly.start, monthEnd)
	monthSub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "supporter-base"}
	monthSub.LineItems[0].AutoRenewingPlan.RecurringPrice = json.RawMessage(`{"currencyCode":"USD","units":"4","nanos":990000000}`)
	connect.AssertEqual(t, monthly.credit(monthSub, monthEnd), true)
	connect.AssertEqual(t, monthly.credit(monthSub, monthEnd.Add(24*time.Hour+time.Second)), true)
	monthSub.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00001..0"
	connect.AssertEqual(t, monthly.credit(monthSub, monthEnd.Add(30*24*time.Hour)), true)
	connect.AssertEqual(t, monthly.texts(), []string{
		playSalesText("Sale", "Pro monthly (new)", "4.99 USD (Google Play list price, before tax and discounts)"),
		playSalesText("Sale", "Pro monthly (renewal)", "4.99 USD (Google Play list price, before tax and discounts)"),
	})
}

// The trial's first charge is the purchase's first sale, after the trial and
// its pending day.
func TestPlayTrialConversionIsTheFirstSale(t *testing.T) {
	fixture := newPlaySalesTrialFixture(t)
	trialEnd := fixture.start.Add(14*24*time.Hour - 2*time.Second)
	sub := playPenguinSubscription(fixture.start, trialEnd)
	connect.AssertEqual(t, fixture.credit(sub, trialEnd), true)
	connect.AssertEqual(t, fixture.credit(sub, trialEnd.Add(24*time.Hour+time.Second)), true)

	// the conversion's order is the first renewal's, "..0"
	sub.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00001..0"
	sub.LatestOrderId = sub.LineItems[0].LatestSuccessfulOrderId
	connect.AssertEqual(t, fixture.credit(sub, trialEnd.Add(365*24*time.Hour)), true)

	connect.AssertEqual(t, fixture.texts(), []string{
		playSalesText("Trial", "Pro yearly (new)", "no charge (free trial until "+trialEnd.Format("2006-01-02")+")"),
		playSalesText("Sale", "Pro yearly (new, after a free trial)", "39.99 USD (Google Play list price, before tax and discounts; offer trial-14d)"),
	})

	// a year later the next charge is a renewal, without the offer
	sub.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00001..1"
	connect.AssertEqual(t, fixture.credit(sub, trialEnd.Add(2*365*24*time.Hour)), true)
	got := fixture.texts()
	connect.AssertEqual(t, got[len(got)-1], playSalesText("Sale", "Pro yearly (renewal)", "39.99 USD (Google Play list price, before tax and discounts)"))
}

// A license tester's purchase is credited and posts nothing: no money moves.
func TestPlayLicenseTesterPurchasePostsNothing(t *testing.T) {
	fixture := newPlaySalesTrialFixture(t)
	trialEnd := fixture.start.Add(14*24*time.Hour - 2*time.Second)
	sub := playPenguinSubscription(fixture.start, trialEnd)
	sub.TestPurchase = json.RawMessage(`{}`)
	connect.AssertEqual(t, fixture.credit(sub, trialEnd), true)
	connect.AssertEqual(t, len(fixture.notices()), 0)
}

func TestClassifyPlayCredit(t *testing.T) {
	start := time.Date(2026, 10, 4, 23, 45, 56, 0, time.UTC)
	day := 24 * time.Hour
	at := func(days float64) time.Time {
		return start.Add(time.Duration(days * float64(day)))
	}
	expiries := func(days ...float64) []time.Time {
		times := []time.Time{}
		for _, d := range days {
			times = append(times, at(d))
		}
		return times
	}
	initial := func() *PlaySubscription {
		return playPenguinSubscription(start, at(14))
	}
	renewal := func() *PlaySubscription {
		sub := initial()
		sub.LineItems[0].LatestSuccessfulOrderId += "..0"
		return sub
	}
	cases := []struct {
		name    string
		sub     *PlaySubscription
		earlier []time.Time
		expiry  time.Time
		want    playCredit
	}{
		{"trial", initial(), nil, at(14), playCreditTrial},
		{"a first month", initial(), nil, at(30), playCreditNew},
		{"a first year", initial(), nil, at(365), playCreditNew},
		{"first credited at a renewal", renewal(), nil, at(61), playCreditRenewal},
		// Main, 2026-10: 14, 15, 16 days, then the hold (not a credit)
		{"trial's pending day", initial(), expiries(14), at(15), playCreditNotCharged},
		{"trial's second pending day", initial(), expiries(14, 15), at(16), playCreditNotCharged},
		{"conversion after the pending day", renewal(), expiries(14, 15), at(379), playCreditAfterTrial},
		{"conversion after two", renewal(), expiries(14, 15, 16), at(380), playCreditAfterTrial},
		// 14, 15, then 21.47 days with revenue: moved, not charged
		{"moved a week", initial(), expiries(14, 15), at(21.47), playCreditNotCharged},
		// monthly: 30, 31, 61, 62, then February
		{"month's pending day", initial(), expiries(30), at(31), playCreditNotCharged},
		{"renewal", renewal(), expiries(30, 31), at(61), playCreditRenewal},
		{"February after the pending day", renewal(), expiries(31, 32), at(59), playCreditRenewal},
		{"153 days, a day, then 5", renewal(), expiries(122, 123, 153, 154), at(158.22), playCreditNotCharged},
		{"a second year", renewal(), expiries(14, 15, 379), at(744), playCreditRenewal},
	}
	for _, c := range cases {
		if got := classifyPlayCredit(c.sub, "supporter", start, c.expiry, c.earlier); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}

	// the phase Play names wins over the window's length
	monthFree := initial()
	monthFree.LineItems[0].OfferPhase = &PlayOfferPhase{FreeTrial: json.RawMessage(`{}`)}
	connect.AssertEqual(t, classifyPlayCredit(monthFree, "supporter", start, at(31), nil), playCreditTrial)
	prorated := initial()
	prorated.LineItems[0].OfferPhase = &PlayOfferPhase{ProrationPeriod: json.RawMessage(`{}`)}
	connect.AssertEqual(t, classifyPlayCredit(prorated, "supporter", start, at(9), nil), playCreditNew)
	// a short first window that continues another purchase is no trial
	linked := initial()
	linked.LinkedPurchaseToken = "token-before"
	connect.AssertEqual(t, classifyPlayCredit(linked, "supporter", start, at(9), nil), playCreditNew)
	// nor is a window of minutes
	connect.AssertEqual(t, classifyPlayCredit(initial(), "supporter", start, start.Add(5*time.Minute), nil), playCreditNew)
}
