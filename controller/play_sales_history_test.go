package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Root-cause tests for where the Play sales notice reads a purchase's earlier
// credits (review R2). It read them from transfer_balance, and retention
// (RemoveCompletedContracts) deletes a balance 7 days after it ends. A trial
// that converts after an account hold had no history left and posted its
// first charge as a "renewal"; a February renewal after a week of Play's
// pending days was measured from a pending day and posted nothing. The
// purchase's subscription_renewal rows are never pruned.
//
// Each seeds the earlier credits as the database holds them after retention:
// every renewal row, and only the balances that ended in the last 7 days.
// The charge then runs through the real credit gate.

// seedPlayCredit writes an earlier credit of the fixture's purchase through
// expiry: its renewal row, and its balance unless retention deleted it.
func (self *playSalesTrialFixture) seedPlayCredit(expiry time.Time, netRevenue model.NanoCents, now time.Time) {
	self.t.Helper()
	ctx := context.Background()
	end := expiry.Add(SubscriptionGracePeriod)
	connect.AssertEqual(self.t, model.AddSubscriptionRenewalInTx(self.tx, ctx, &model.SubscriptionRenewal{
		NetworkId:          self.args.NetworkId,
		StartTime:          self.start,
		EndTime:            end,
		NetRevenue:         netRevenue,
		PurchaseToken:      self.args.PurchaseToken,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		SubscriptionMarket: model.SubscriptionMarketGoogle,
	}), nil)
	if end.Before(now.Add(-7 * 24 * time.Hour)) {
		// RemoveCompletedContracts deleted the balance
		return
	}
	byteCount := legacyActivationTestByteCount
	if netRevenue == 0 {
		// a grace period's hold
		byteCount = 0
	}
	model.AddTransferBalanceInTx(ctx, self.tx, &model.TransferBalance{
		NetworkId:             self.args.NetworkId,
		StartTime:             self.start,
		EndTime:               end,
		StartBalanceByteCount: byteCount,
		BalanceByteCount:      byteCount,
		SubsidyNetRevenue:     netRevenue,
		PurchaseToken:         self.args.PurchaseToken,
		Pro:                   true,
	})
}

// A trial whose first charge failed, then recovered from Play's account hold
// 23 days after the trial ended: every balance of the trial is deleted. The
// recovered charge is the purchase's first sale.
func TestPlayConversionAfterAccountHoldIsTheFirstSale(t *testing.T) {
	fixture := newPlaySalesTrialFixture(t)
	now := server.NowUtc()
	day := 24 * time.Hour
	fixture.start = now.Add(-40 * day).Truncate(time.Second)
	trialEnd := fixture.start.Add(14*day - 2*time.Second)
	playRevenue := model.UsdToNanoCents(3.5)
	// the trial, Play's pending day, then the grace period's hold
	fixture.seedPlayCredit(trialEnd, playRevenue, now)
	fixture.seedPlayCredit(trialEnd.Add(day+time.Second), playRevenue, now)
	fixture.seedPlayCredit(fixture.start.Add(21*day), 0, now)

	// the account hold ended with the charge: a year from the recovery
	sub := playPenguinSubscription(fixture.start, now.Add(365*day))
	sub.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00001..0"
	sub.LatestOrderId = sub.LineItems[0].LatestSuccessfulOrderId
	connect.AssertEqual(t, fixture.credit(sub, now.Add(365*day).Truncate(time.Second)), true)

	connect.AssertEqual(t, fixture.texts(), []string{
		playSalesText("Sale", "Pro yearly (new, after a free trial)", "39.99 USD (Google Play list price, before tax and discounts; offer trial-14d)"),
	})
}

// A monthly purchase whose February renewal was pending for 8 days: Play
// moved the expiry out a day at a time, and kept the billing anchor. The
// first month's balance is deleted by then. The renewal lands 28 days after
// the last charged window and 19 days after the last pending day: a sale.
func TestPlayFebruaryRenewalAfterAWeekOfPendingDaysIsASale(t *testing.T) {
	fixture := newPlaySalesTrialFixture(t)
	now := server.NowUtc()
	day := 24 * time.Hour
	fixture.start = now.Add(-39*day - time.Hour).Truncate(time.Second)
	monthEnd := fixture.start.Add(31 * day)
	playRevenue := model.UsdToNanoCents(3.5)
	fixture.seedPlayCredit(monthEnd, playRevenue, now)
	for pending := 1; pending <= 8; pending += 1 {
		fixture.seedPlayCredit(monthEnd.Add(time.Duration(pending)*(day+time.Second)), playRevenue, now)
	}

	sub := playPenguinSubscription(fixture.start, monthEnd)
	sub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "supporter-base"}
	sub.LineItems[0].AutoRenewingPlan.RecurringPrice = json.RawMessage(`{"currencyCode":"USD","units":"4","nanos":990000000}`)
	sub.LineItems[0].LatestSuccessfulOrderId = "GPA.3300-0000-0000-00001..0"
	// February: the renewal keeps the anchor, 28 days after the paid month
	connect.AssertEqual(t, fixture.credit(sub, monthEnd.Add(28*day)), true)

	connect.AssertEqual(t, fixture.texts(), []string{
		playSalesText("Sale", "Pro monthly (renewal)", "4.99 USD (Google Play list price, before tax and discounts)"),
	})
}
