package controller

// The buyer's report of a card payment through the dialog's poll of its order
// (GiftOrderStatus, giftReportCardOrder): a pending card order's session is
// read from Stripe at most once per giftCheckoutReportInterval, and a paid
// one is fulfilled through the webhook's path. Needs the test database and
// its Redis (server.DefaultTestEnv); Stripe is the reconciler's fake.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// giftReportTestOrder stores a pending card order of one gift keyed to
// sessionId, and answers it with its token.
func giftReportTestOrder(t testing.TB, ctx context.Context, sessionId string) *model.GiftOrder {
	unitPriceCents := model.Gift().UnitPriceCents(1)
	order := &model.GiftOrder{
		Quantity:       1,
		UnitPriceCents: unitPriceCents,
		TotalCents:     unitPriceCents,
		Rail:           model.GiftRailStripe,
		Delivery:       model.GiftDeliveryMe,
		BuyerEmail:     "buyer@synthetic.example",
		Language:       "en",
	}
	connect.AssertEqual(t, model.CreateGiftOrder(ctx, order), nil)
	connect.AssertEqual(t, model.SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, sessionId), nil)
	order.StripeSessionId = sessionId
	return order
}

func giftReportTestPoll(t testing.TB, order *model.GiftOrder) *GiftOrderStatusResult {
	status, err := GiftOrderStatus(
		&GiftOrderStatusArgs{OrderId: order.GiftOrderId.String(), Token: order.OrderToken},
		giftPurchaseTestSession(),
	)
	connect.AssertEqual(t, err, nil)
	return status
}

// A paid session is fulfilled by the buyer's poll, once, through the webhook's
// path; an open one waits, and the gate lets one poll read it per interval; a
// session Stripe cannot answer leaves the order as stored.
func TestGiftOrderStatusReportsCardPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		env := newStripeReconcileTestEnv(t)

		paid := giftReportTestOrder(t, ctx, "cs_giftreport_paid")
		paidSession := giftReconcileTestSession(model.GetGiftOrder(ctx, paid.GiftOrderId), "complete", "paid")
		env.setCheckoutSession(paid.StripeSessionId, paidSession)
		giftPurchaseTestResetLimiters()
		status := giftReportTestPoll(t, paid)
		connect.AssertEqual(t, status.Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, env.checkoutSessionReadCount(paid.StripeSessionId), 1)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, paid.GiftOrderId), 1)
		// a paid order is answered as stored: no more reads, and the late
		// webhook makes nothing more
		status = giftReportTestPoll(t, paid)
		connect.AssertEqual(t, status.Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, env.checkoutSessionReadCount(paid.StripeSessionId), 1)
		_, err := StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", paidSession), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, paid.GiftOrderId), 1)
		connect.AssertEqual(t, giftReconcileTestRenderTaskCount(t, ctx, paid.GiftOrderId), 1)

		// open: read once, then the gate holds the next polls
		open := giftReportTestOrder(t, ctx, "cs_giftreport_open")
		env.setCheckoutSession(open.StripeSessionId, giftReconcileTestSession(model.GetGiftOrder(ctx, open.GiftOrderId), "open", "unpaid"))
		for range 3 {
			connect.AssertEqual(t, giftReportTestPoll(t, open).Status, model.GiftOrderStatusPending)
		}
		connect.AssertEqual(t, env.checkoutSessionReadCount(open.StripeSessionId), 1)
		// paid since, while the gate still holds: answered as stored until the
		// interval passes (the gate is opened here, as time would)
		env.setCheckoutSession(open.StripeSessionId, giftReconcileTestSession(model.GetGiftOrder(ctx, open.GiftOrderId), "complete", "paid"))
		connect.AssertEqual(t, giftReportTestPoll(t, open).Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, giftCheckoutReportInterval, 20*time.Second)
		previousGate := giftCheckoutReportGateFunc
		giftCheckoutReportGateFunc = func(context.Context, server.Id) bool { return true }
		t.Cleanup(func() { giftCheckoutReportGateFunc = previousGate })
		connect.AssertEqual(t, giftReportTestPoll(t, open).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, env.checkoutSessionReadCount(open.StripeSessionId), 2)

		// Stripe fails: the poll answers the order as stored, without an error
		failing := giftReportTestOrder(t, ctx, "cs_giftreport_failing")
		env.failCheckoutSession(failing.StripeSessionId, http.StatusInternalServerError)
		connect.AssertEqual(t, giftReportTestPoll(t, failing).Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, env.checkoutSessionReadCount(failing.StripeSessionId), 1)

		// a mismatched payment is recorded, not fulfilled
		short := giftReportTestOrder(t, ctx, "cs_giftreport_short")
		shortSession := giftReconcileTestSession(model.GetGiftOrder(ctx, short.GiftOrderId), "complete", "paid")
		shortSession["amount_total"] = short.TotalCents - 1
		env.setCheckoutSession(short.StripeSessionId, shortSession)
		connect.AssertEqual(t, giftReportTestPoll(t, short).Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, short.GiftOrderId), 0)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketGift, model.PaymentReconcileActionCreditUnfulfillable, short.StripeSessionId), 1)

		// paid, and its charge disputed or refunded since: never fulfilled by
		// the poll (the hourly reconciler records it)
		for _, change := range []func(charge map[string]any){
			func(charge map[string]any) { charge["disputed"] = true },
			func(charge map[string]any) { charge["amount_refunded"] = 100 },
		} {
			order := giftReportTestOrder(t, ctx, "cs_giftreport_kept_"+server.NewId().String())
			session := giftReconcileTestSession(model.GetGiftOrder(ctx, order.GiftOrderId), "complete", "paid")
			change(giftReconcileTestCharge(session))
			env.setCheckoutSession(order.StripeSessionId, session)
			connect.AssertEqual(t, giftReportTestPoll(t, order).Status, model.GiftOrderStatusPending)
			connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 1)
			connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 0)
		}
	})
}
