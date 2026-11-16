package controller

// The Stripe store's gift leg of the payment reconciliation
// (payment_reconcile_gift.go): card gift orders still pending whose Checkout
// session Stripe says is paid. The reconciler's fake Stripe
// (newStripeReconcileTestEnv) answers the session reads, and every run is
// held to one PostgreSQL session by the reconciler's connection guard
// (disableAllReconcileStores). Needs the test database
// (server.DefaultTestEnv).

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// newGiftReconcileTest is the reconciler with only the Stripe store on, a
// fake Stripe and the gift config. The caller holds the reconciler to one
// PostgreSQL session first (defer disableAllReconcileStores(t)()), so the
// guard is closed before the test database is dropped.
func newGiftReconcileTest(t testing.TB) *stripeReconcileTestEnv {
	giftPurchaseTestConfig(t)
	return newStripeReconcileTestEnv(t)
}

// giftReconcileTestOrder stores a pending card order of quantity gifts, all
// to the buyer, keyed to sessionId and made age ago.
func giftReconcileTestOrder(t testing.TB, ctx context.Context, sessionId string, quantity int, age time.Duration) *model.GiftOrder {
	unitPriceCents := model.Gift().UnitPriceCents(quantity)
	order := &model.GiftOrder{
		Quantity:       quantity,
		UnitPriceCents: unitPriceCents,
		TotalCents:     unitPriceCents * int64(quantity),
		Rail:           model.GiftRailStripe,
		Delivery:       model.GiftDeliveryMe,
		BuyerEmail:     "buyer@synthetic.example",
		Language:       "en",
	}
	connect.AssertEqual(t, model.CreateGiftOrder(ctx, order), nil)
	connect.AssertEqual(t, model.SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, sessionId), nil)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gift_order SET create_time = $2 WHERE gift_order_id = $1`,
			order.GiftOrderId,
			server.NowUtc().Add(-age),
		))
	})
	return model.GetGiftOrder(ctx, order.GiftOrderId)
}

// giftReconcileTestSession is the order's Checkout session as Stripe answers
// a read of it, for the order's total in usd, with its payment intent and the
// charge that paid it, kept whole (the fake answers them expanded only to a
// read that expands them).
func giftReconcileTestSession(order *model.GiftOrder, status string, paymentStatus string) map[string]any {
	return map[string]any{
		"id":             order.StripeSessionId,
		"object":         "checkout.session",
		"mode":           "payment",
		"status":         status,
		"payment_status": paymentStatus,
		"amount_total":   order.TotalCents,
		"currency":       "usd",
		"payment_intent": map[string]any{
			"id":     "pi_" + order.StripeSessionId,
			"object": "payment_intent",
			"latest_charge": map[string]any{
				"id":              "ch_" + order.StripeSessionId,
				"object":          "charge",
				"status":          "succeeded",
				"amount":          order.TotalCents,
				"amount_refunded": 0,
				"refunded":        false,
				"disputed":        false,
			},
		},
		"metadata":         map[string]string{"gift_order_id": order.GiftOrderId.String()},
		"customer_details": map[string]any{"email": order.BuyerEmail},
		"created":          order.CreateTime.Unix(),
	}
}

// The charge of a session giftReconcileTestSession made, to change.
func giftReconcileTestCharge(session map[string]any) map[string]any {
	return session["payment_intent"].(map[string]any)["latest_charge"].(map[string]any)
}

// giftReconcileTestRun is one pass of the Stripe store.
func giftReconcileTestRun(t testing.TB, ctx context.Context, dryRun bool) *PaymentReconcileRunResult {
	result, err := RunPaymentReconciliationWithOptions(
		reconcileTestSession(t, ctx),
		&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketStripe}, DryRun: dryRun},
	)
	connect.AssertEqual(t, err, nil)
	return result
}

// The gift leg's reads counted so far with result.
func giftReconcileTestCount(result string) float64 {
	return testutil.ToFloat64(giftCheckoutReconcileResults.WithLabelValues(result))
}

// The stripe store's details on the run's heartbeat.
func giftReconcileTestStripeDetails(t testing.TB, ctx context.Context, runId server.Id) map[string]any {
	for _, event := range model.GetPaymentReconciliationEvents(ctx, runId) {
		if event.Store != model.PaymentReconcileStoreAll || event.Action != model.PaymentReconcileActionHeartbeat {
			continue
		}
		stores, _ := event.Details["stores"].(map[string]any)
		details, _ := stores[model.SubscriptionMarketStripe].(map[string]any)
		if details == nil {
			return map[string]any{}
		}
		return details
	}
	t.Fatalf("run %s has no heartbeat", runId)
	return nil
}

// The run's events of the store, action and evidence.
func giftReconcileTestEvents(ctx context.Context, runId server.Id, store string, action string, evidence string) []*model.PaymentReconciliationEvent {
	events := []*model.PaymentReconciliationEvent{}
	for _, event := range model.GetPaymentReconciliationEvents(ctx, runId) {
		if event.Store == store && event.Action == action && event.Evidence == evidence {
			events = append(events, event)
		}
	}
	return events
}

// A paid session whose webhook never came is fulfilled once through the
// webhook's path: the order is paid, its codes made under the session, its
// delivery scheduled and its sales notice registered, and the run records the
// credit. The webhook delivered afterwards, as either paid event, makes
// nothing more, and the next run does not read the session again.
func TestPaymentReconcileGiftCheckoutPaidFulfilsOnceAndLaterWebhookDoesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)
		notices, restoreNotices := model.Testing_CaptureSalesPaymentNotices()
		t.Cleanup(restoreNotices)

		order := giftReconcileTestOrder(t, ctx, "cs_giftrec_paid", 2, time.Hour)
		session := giftReconcileTestSession(order, "complete", "paid")
		env.setCheckoutSession(order.StripeSessionId, session)
		fulfilledBefore := giftReconcileTestCount(giftCheckoutReconcileResultFulfilled)

		result := giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].Credited, 1)
		connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 1)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultFulfilled)-fulfilledBefore, float64(1))

		credited := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited, order.StripeSessionId)
		connect.AssertEqual(t, len(credited), 1)
		connect.AssertEqual(t, credited[0].Details["leg"], stripeGiftCheckoutLegName)
		connect.AssertEqual(t, credited[0].Details["gift_order"], order.GiftOrderId.String())
		details := giftReconcileTestStripeDetails(t, ctx, result.RunId)
		connect.AssertEqual(t, details["gift_checkout_fulfilled"], float64(1))
		connect.AssertEqual(t, details["gift_checkout_calls"], float64(1))

		paid := model.GetGiftOrder(ctx, order.GiftOrderId)
		connect.AssertEqual(t, paid.Status, model.GiftOrderStatusPaid)
		codes := model.GetGiftOrderCodes(ctx, order.GiftOrderId)
		connect.AssertEqual(t, len(codes), 2)
		for n, code := range codes {
			balanceCode, err := model.GetBalanceCode(ctx, code.BalanceCodeId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, balanceCode.PurchaseEventId, fmt.Sprintf("cs_giftrec_paid/gift/%d", n))
		}
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 2)
		// the cards and the emails, scheduled with the codes
		connect.AssertEqual(t, giftReconcileTestRenderTaskCount(t, ctx, order.GiftOrderId), 1)

		// the webhook arrives late, as either paid event: nothing more is made
		webhookSession := reconcileTestSession(t, ctx)
		for _, eventType := range []string{"checkout.session.completed", "checkout.session.async_payment_succeeded"} {
			_, err := StripeWebhook(stripeWebhookEvent(t, eventType, session), webhookSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 2)
		}
		connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).PaidTime.Equal(*paid.PaidTime), true)
		connect.AssertEqual(t, giftReconcileTestRenderTaskCount(t, ctx, order.GiftOrderId), 1)
		// one sales notice for the order, whichever reported it
		giftNotices := 0
		for _, notice := range notices() {
			if notice.RecordKey == "gift_order/"+order.GiftOrderId.String() {
				giftNotices += 1
			}
		}
		connect.AssertEqual(t, giftNotices, 1)

		// a paid order is not pending: the next run reads nothing
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 1)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited, order.StripeSessionId), 1)
	})
}

// The gift order's render task (ScheduleGiftDeliveryInTx), pending or done.
func giftReconcileTestRenderTaskCount(t testing.TB, ctx context.Context, giftOrderId server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT count(*)
				FROM pending_task
				WHERE function_name LIKE '%GiftRenderCards' AND args_json::jsonb ->> 'gift_order_id' = $1
			`,
			giftOrderId.String(),
		).Scan(&count))
	})
	return count
}

// An order the webhook already fulfilled is not pending: the reconciler
// reads nothing and records nothing.
func TestPaymentReconcileGiftCheckoutAfterWebhookHasNothingToDo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)

		order := giftReconcileTestOrder(t, ctx, "cs_giftrec_webhook", 1, time.Hour)
		session := giftReconcileTestSession(order, "complete", "paid")
		env.setCheckoutSession(order.StripeSessionId, session)
		_, err := StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", session), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 1)
		fulfilledBefore := giftReconcileTestCount(giftCheckoutReconcileResultFulfilled)

		result := giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 0)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 1)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited, order.StripeSessionId), 0)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultFulfilled)-fulfilledBefore, float64(0))
		_, read := giftReconcileTestStripeDetails(t, ctx, result.RunId)["gift_checkout_calls"]
		connect.AssertEqual(t, read, false)
	})
}

// An open session waits, and is read again every run; completed with a
// delayed payment method it waits for the money; once paid, it is fulfilled.
func TestPaymentReconcileGiftCheckoutOpenSessionWaits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)

		order := giftReconcileTestOrder(t, ctx, "cs_giftrec_open", 3, time.Hour)
		env.setCheckoutSession(order.StripeSessionId, giftReconcileTestSession(order, "open", "unpaid"))
		openBefore := giftReconcileTestCount(giftCheckoutReconcileResultOpen)
		unpaidBefore := giftReconcileTestCount(giftCheckoutReconcileResultUnpaid)

		for run := 1; run <= 2; run += 1 {
			result := giftReconcileTestRun(t, ctx, false)
			connect.AssertEqual(t, result.Errors, 0)
			connect.AssertEqual(t, result.Credited, 0)
			connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), run)
			connect.AssertEqual(t, giftReconcileTestStripeDetails(t, ctx, result.RunId)["gift_checkout_open"], float64(1))
			connect.AssertEqual(t, len(model.GetPaymentReconciliationEvents(ctx, result.RunId)), 1)
		}
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultOpen)-openBefore, float64(2))
		connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPending)

		// a bank debit completed the session; its money has not cleared
		env.setCheckoutSession(order.StripeSessionId, giftReconcileTestSession(order, "complete", "unpaid"))
		result := giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultUnpaid)-unpaidBefore, float64(1))
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 0)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPending)

		// the money cleared, and async_payment_succeeded never came
		env.setCheckoutSession(order.StripeSessionId, giftReconcileTestSession(order, "complete", "paid"))
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 3)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 4)
	})
}

// An expired session is recorded once and not read again; neither is a
// session whose delayed payment the webhook recorded as failed. The order
// stays pending, as an abandoned order does.
func TestPaymentReconcileGiftCheckoutExpiredSessionStopsBeingChecked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)

		expired := giftReconcileTestOrder(t, ctx, "cs_giftrec_expired", 1, 30*time.Hour)
		env.setCheckoutSession(expired.StripeSessionId, giftReconcileTestSession(expired, "expired", "unpaid"))
		failed := giftReconcileTestOrder(t, ctx, "cs_giftrec_failed", 1, 40*time.Hour)
		failedSession := giftReconcileTestSession(failed, "complete", "unpaid")
		env.setCheckoutSession(failed.StripeSessionId, failedSession)
		expiredBefore := giftReconcileTestCount(giftCheckoutReconcileResultExpired)

		// the delayed payment of the second failed, and the webhook said so
		_, err := StripeWebhook(stripeWebhookEvent(t, "checkout.session.async_payment_failed", failedSession), reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionPaymentFailed, failed.StripeSessionId), 1)

		// a dry run records only its own dry-run row, and does not settle it
		result := giftReconcileTestRun(t, ctx, true)
		connect.AssertEqual(t, result.Errors, 0)
		dryRows := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionCheckoutExpired, expired.StripeSessionId)
		connect.AssertEqual(t, len(dryRows), 1)
		connect.AssertEqual(t, dryRows[0].DryRun, true)
		connect.AssertEqual(t, env.checkoutSessionReadCount(expired.StripeSessionId), 1)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultExpired)-expiredBefore, float64(0))

		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(expired.StripeSessionId), 2)
		connect.AssertEqual(t, env.checkoutSessionReadCount(failed.StripeSessionId), 0)
		recorded := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionCheckoutExpired, expired.StripeSessionId)
		connect.AssertEqual(t, len(recorded), 1)
		connect.AssertEqual(t, recorded[0].DryRun, false)
		connect.AssertEqual(t, recorded[0].Details["leg"], stripeGiftCheckoutLegName)
		connect.AssertEqual(t, recorded[0].Details["gift_order"], expired.GiftOrderId.String())
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultExpired)-expiredBefore, float64(1))

		// settled: neither is read again
		for range 2 {
			result = giftReconcileTestRun(t, ctx, false)
			connect.AssertEqual(t, result.Errors, 0)
		}
		connect.AssertEqual(t, env.checkoutSessionReadCount(expired.StripeSessionId), 2)
		connect.AssertEqual(t, env.checkoutSessionReadCount(failed.StripeSessionId), 0)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCheckoutExpired, expired.StripeSessionId), 2)
		for _, order := range []*model.GiftOrder{expired, failed} {
			connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPending)
			connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 0)
		}
	})
}

// A refunded order is not pending, so it is not read; one refunded while its
// session is being read is not fulfilled, as the webhook does.
func TestPaymentReconcileGiftCheckoutSkipsRefundedOrders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)
		refund := func(giftOrderId server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(model.SetGiftOrderRefundedInTx(tx, ctx, giftOrderId))
			})
		}

		refunded := giftReconcileTestOrder(t, ctx, "cs_giftrec_refunded", 1, time.Hour)
		env.setCheckoutSession(refunded.StripeSessionId, giftReconcileTestSession(refunded, "complete", "paid"))
		refund(refunded.GiftOrderId)

		racing := giftReconcileTestOrder(t, ctx, "cs_giftrec_racing", 2, 2*time.Hour)
		env.setCheckoutSession(racing.StripeSessionId, giftReconcileTestSession(racing, "complete", "paid"))
		// the refund lands while the reconciler reads the session
		env.checkoutSessionsLock.Lock()
		env.onCheckoutSessionRead = func(sessionId string) {
			if sessionId == racing.StripeSessionId {
				refund(racing.GiftOrderId)
			}
		}
		env.checkoutSessionsLock.Unlock()
		refundedBefore := giftReconcileTestCount(giftCheckoutReconcileResultRefunded)

		result := giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(refunded.StripeSessionId), 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(racing.StripeSessionId), 1)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultRefunded)-refundedBefore, float64(1))
		for _, order := range []*model.GiftOrder{refunded, racing} {
			connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusRefunded)
			connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 0)
			connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited, order.StripeSessionId), 0)
		}

		// refunded orders are not read again
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(racing.StripeSessionId), 1)
	})
}

// A paid session of another amount or currency is recorded once through the
// webhook's path (store gift, reason gift_payment_mismatch) and never
// fulfilled, and is not read again; a dry run records only its dry-run row.
func TestPaymentReconcileGiftCheckoutRecordsMismatchWithoutFulfilling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)

		short := giftReconcileTestOrder(t, ctx, "cs_giftrec_short", 1, time.Hour)
		shortSession := giftReconcileTestSession(short, "complete", "paid")
		shortSession["amount_total"] = short.TotalCents - 1
		env.setCheckoutSession(short.StripeSessionId, shortSession)
		euros := giftReconcileTestOrder(t, ctx, "cs_giftrec_eur", 1, 2*time.Hour)
		eurSession := giftReconcileTestSession(euros, "complete", "paid")
		eurSession["currency"] = "eur"
		env.setCheckoutSession(euros.StripeSessionId, eurSession)
		mismatchBefore := giftReconcileTestCount(giftCheckoutReconcileResultMismatch)

		result := giftReconcileTestRun(t, ctx, true)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)
		for _, order := range []*model.GiftOrder{short, euros} {
			rows := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketGift, model.PaymentReconcileActionCreditUnfulfillable, order.StripeSessionId)
			connect.AssertEqual(t, len(rows), 1)
			connect.AssertEqual(t, rows[0].DryRun, true)
		}

		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultMismatch)-mismatchBefore, float64(2))
		for _, order := range []*model.GiftOrder{short, euros} {
			connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPending)
			connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 0)
			rows := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketGift, model.PaymentReconcileActionCreditUnfulfillable, order.StripeSessionId)
			connect.AssertEqual(t, len(rows), 1)
			connect.AssertEqual(t, rows[0].DryRun, false)
			connect.AssertEqual(t, rows[0].Details["reason"], "gift_payment_mismatch")
			connect.AssertEqual(t, rows[0].Details["leg"], stripeGiftCheckoutLegName)
			connect.AssertEqual(t, rows[0].Details["gift_order"], order.GiftOrderId.String())
			connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited, order.StripeSessionId), 0)
		}
		connect.AssertEqual(t, giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketGift, model.PaymentReconcileActionCreditUnfulfillable, short.StripeSessionId)[0].Details["amount_total"], float64(short.TotalCents-1))

		// for an operator now: not read again, and recorded once
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		for _, order := range []*model.GiftOrder{short, euros} {
			connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 2)
			connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketGift, model.PaymentReconcileActionCreditUnfulfillable, order.StripeSessionId), 2)
		}
	})
}

// Only orders made inside the window, and before the grace, are read: one
// made 22 days ago and one made five minutes ago are not, and one made 20
// days ago is fulfilled.
func TestPaymentReconcileGiftCheckoutWindowBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)

		tooOld := giftReconcileTestOrder(t, ctx, "cs_giftrec_old", 1, 22*24*time.Hour)
		inside := giftReconcileTestOrder(t, ctx, "cs_giftrec_inside", 1, 20*24*time.Hour)
		tooNew := giftReconcileTestOrder(t, ctx, "cs_giftrec_new", 1, 5*time.Minute)
		for _, order := range []*model.GiftOrder{tooOld, inside, tooNew} {
			env.setCheckoutSession(order.StripeSessionId, giftReconcileTestSession(order, "complete", "paid"))
		}

		result := giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, env.checkoutSessionReadCount(tooOld.StripeSessionId), 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(inside.StripeSessionId), 1)
		connect.AssertEqual(t, env.checkoutSessionReadCount(tooNew.StripeSessionId), 0)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, inside.GiftOrderId).Status, model.GiftOrderStatusPaid)
		for _, order := range []*model.GiftOrder{tooOld, tooNew} {
			connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPending)
		}
		connect.AssertEqual(t, giftCheckoutReconcileWindow, 21*24*time.Hour)
		connect.AssertEqual(t, giftCheckoutReconcileGrace, 15*time.Minute)
	})
}

// A failed read and a session Stripe does not know are each the order's
// error, and the next order is read and fulfilled: one bad order does not
// stop the rest. An order's error is a stripe error row the store-error alert
// reads, counted in the run's errors but never as a store error, so the
// invoice listing's watermark advances beside it. Reads failing in a row stop
// the leg until the next run.
func TestPaymentReconcileGiftCheckoutStripeErrors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)
		errorBefore := giftReconcileTestCount(giftCheckoutReconcileResultError)

		failing := giftReconcileTestOrder(t, ctx, "cs_giftrec_failing", 1, 4*time.Hour)
		env.failCheckoutSession(failing.StripeSessionId, http.StatusInternalServerError)
		unknown := giftReconcileTestOrder(t, ctx, "cs_giftrec_unknown", 1, 3*time.Hour)
		paid := giftReconcileTestOrder(t, ctx, "cs_giftrec_after", 1, 2*time.Hour)
		env.setCheckoutSession(paid.StripeSessionId, giftReconcileTestSession(paid, "complete", "paid"))

		result := giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 2)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].Errors, 0)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].GiftCheckoutErrors, 2)
		connect.AssertEqual(t, giftReconcileTestStripeDetails(t, ctx, result.RunId)["gift_checkout_errors"], float64(2))
		connect.AssertEqual(t, model.GetGiftOrder(ctx, paid.GiftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultError)-errorBefore, float64(2))
		readError := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionError, failing.StripeSessionId)
		connect.AssertEqual(t, len(readError), 1)
		connect.AssertEqual(t, readError[0].Details["reason"], "read")
		connect.AssertEqual(t, readError[0].Details["leg"], stripeGiftCheckoutLegName)
		notFound := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionError, unknown.StripeSessionId)
		connect.AssertEqual(t, len(notFound), 1)
		connect.AssertEqual(t, notFound[0].Details["reason"], "session_not_found")
		// the orders' errors never hold the invoice listing's watermark
		firstWatermark, watermarked := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, watermarked, true)

		// both failures are read again next run; once Stripe answers, the
		// errors stop
		env.setCheckoutSession(failing.StripeSessionId, giftReconcileTestSession(failing, "open", "unpaid"))
		env.setCheckoutSession(unknown.StripeSessionId, giftReconcileTestSession(unknown, "expired", "unpaid"))
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(failing.StripeSessionId), 2)
		connect.AssertEqual(t, env.checkoutSessionReadCount(unknown.StripeSessionId), 2)
		secondWatermark, _ := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, secondWatermark.After(firstWatermark), true)

		// Stripe fails every read: two in a row stop the leg, newest first,
		// and the watermark still advances
		previousMaxErrors := giftCheckoutReconcileMaxConsecutiveErrors
		giftCheckoutReconcileMaxConsecutiveErrors = 2
		t.Cleanup(func() { giftCheckoutReconcileMaxConsecutiveErrors = previousMaxErrors })
		outage := []*model.GiftOrder{}
		for n := range 3 {
			order := giftReconcileTestOrder(t, ctx, fmt.Sprintf("cs_giftrec_outage_%d", n), 1, time.Duration(70-n)*time.Minute)
			env.failCheckoutSession(order.StripeSessionId, http.StatusServiceUnavailable)
			outage = append(outage, order)
		}
		env.failCheckoutSession(failing.StripeSessionId, http.StatusServiceUnavailable)
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 2)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].Errors, 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(outage[2].StripeSessionId), 1)
		connect.AssertEqual(t, env.checkoutSessionReadCount(outage[1].StripeSessionId), 1)
		connect.AssertEqual(t, env.checkoutSessionReadCount(outage[0].StripeSessionId), 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(failing.StripeSessionId), 2)
		connect.AssertEqual(t, giftReconcileTestStripeDetails(t, ctx, result.RunId)["gift_checkout_stopped_on_errors"], true)
		thirdWatermark, _ := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, thirdWatermark.After(secondWatermark), true)
	})
}

// A burst of open sessions (a card-testing wave) larger than the cap does not
// keep a recent payment waiting: the newest pending orders are read first, and
// the cap keeps a reserve for the oldest, so they are reached too.
func TestPaymentReconcileGiftCheckoutReachesARecentPaymentInABurst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)
		previousMaxCalls, previousOldestCalls := giftCheckoutReconcileMaxCalls, giftCheckoutReconcileOldestCalls
		giftCheckoutReconcileMaxCalls, giftCheckoutReconcileOldestCalls = 4, 1
		t.Cleanup(func() {
			giftCheckoutReconcileMaxCalls, giftCheckoutReconcileOldestCalls = previousMaxCalls, previousOldestCalls
		})

		// six open sessions, ten to five hours old, and a payment from half an
		// hour ago whose webhook never came
		burst := []*model.GiftOrder{}
		for hours := 10; 5 <= hours; hours -= 1 {
			order := giftReconcileTestOrder(t, ctx, fmt.Sprintf("cs_giftrec_burst_%d", hours), 1, time.Duration(hours)*time.Hour)
			env.setCheckoutSession(order.StripeSessionId, giftReconcileTestSession(order, "open", "unpaid"))
			burst = append(burst, order)
		}
		paid := giftReconcileTestOrder(t, ctx, "cs_giftrec_burst_paid", 1, 30*time.Minute)
		env.setCheckoutSession(paid.StripeSessionId, giftReconcileTestSession(paid, "complete", "paid"))

		result := giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, paid.GiftOrderId).Status, model.GiftOrderStatusPaid)
		details := giftReconcileTestStripeDetails(t, ctx, result.RunId)
		connect.AssertEqual(t, details["gift_checkout_calls"], float64(4))
		connect.AssertEqual(t, details["gift_checkout_calls_exhausted"], true)
		// the newest three (the payment, five and six hours) and the oldest
		connect.AssertEqual(t, env.checkoutSessionReadCount(paid.StripeSessionId), 1)
		reads := map[int]int{}
		for i, order := range burst {
			reads[10-i] = env.checkoutSessionReadCount(order.StripeSessionId)
		}
		connect.AssertEqual(t, reads, map[int]int{10: 1, 9: 0, 8: 0, 7: 0, 6: 1, 5: 1})

		// the next run moves on through the burst from both ends
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		for i, order := range burst {
			reads[10-i] = env.checkoutSessionReadCount(order.StripeSessionId)
		}
		connect.AssertEqual(t, reads, map[int]int{10: 2, 9: 0, 8: 0, 7: 1, 6: 2, 5: 2})
	})
}

// A paid session whose charge was refunded (in whole or in part) or disputed
// is never fulfilled: a dispute of an order with no codes voids nothing, so
// its codes would go live mid-dispute, and a refund the webhook missed would
// be given away. Each is recorded once (store stripe, refunded or disputed)
// and not read again. A charge that has not settled, or that cannot be read,
// is the order's error, read again next run. A dry run records only its
// dry-run rows.
func TestPaymentReconcileGiftCheckoutRefundedOrDisputedChargeIsNotFulfilled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)
		refundedBefore := giftReconcileTestCount(giftCheckoutReconcileResultRefunded)
		disputedBefore := giftReconcileTestCount(giftCheckoutReconcileResultDisputed)

		paidSession := func(order *model.GiftOrder, change func(charge map[string]any)) map[string]any {
			session := giftReconcileTestSession(order, "complete", "paid")
			change(giftReconcileTestCharge(session))
			return session
		}
		refunded := giftReconcileTestOrder(t, ctx, "cs_giftrec_charge_refunded", 1, time.Hour)
		env.setCheckoutSession(refunded.StripeSessionId, paidSession(refunded, func(charge map[string]any) {
			charge["amount_refunded"] = refunded.TotalCents
			charge["refunded"] = true
		}))
		partial := giftReconcileTestOrder(t, ctx, "cs_giftrec_charge_partial", 3, 2*time.Hour)
		env.setCheckoutSession(partial.StripeSessionId, paidSession(partial, func(charge map[string]any) {
			charge["amount_refunded"] = partial.UnitPriceCents
		}))
		disputed := giftReconcileTestOrder(t, ctx, "cs_giftrec_charge_disputed", 1, 3*time.Hour)
		env.setCheckoutSession(disputed.StripeSessionId, paidSession(disputed, func(charge map[string]any) {
			charge["disputed"] = true
		}))
		pending := giftReconcileTestOrder(t, ctx, "cs_giftrec_charge_pending", 1, 4*time.Hour)
		env.setCheckoutSession(pending.StripeSessionId, paidSession(pending, func(charge map[string]any) {
			charge["status"] = "pending"
		}))
		unexpanded := giftReconcileTestOrder(t, ctx, "cs_giftrec_charge_unexpanded", 1, 5*time.Hour)
		unexpandedSession := giftReconcileTestSession(unexpanded, "complete", "paid")
		unexpandedSession["payment_intent"] = "pi_" + unexpanded.StripeSessionId
		env.setCheckoutSession(unexpanded.StripeSessionId, unexpandedSession)

		// a dry run: its own rows, nothing settled
		result := giftReconcileTestRun(t, ctx, true)
		connect.AssertEqual(t, result.Credited, 0)
		dryRows := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionDisputed, disputed.StripeSessionId)
		connect.AssertEqual(t, len(dryRows), 1)
		connect.AssertEqual(t, dryRows[0].DryRun, true)

		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Credited, 0)
		// the unsettled and the unreadable charge: the orders' errors only
		connect.AssertEqual(t, result.Errors, 2)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].Errors, 0)
		for _, check := range []struct {
			order  *model.GiftOrder
			reason string
		}{{pending, "charge_not_settled"}, {unexpanded, "charge"}} {
			rows := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionError, check.order.StripeSessionId)
			connect.AssertEqual(t, len(rows), 1)
			connect.AssertEqual(t, rows[0].Details["reason"], check.reason)
		}
		for _, check := range []struct {
			order  *model.GiftOrder
			action string
		}{
			{refunded, model.PaymentReconcileActionRefunded},
			{partial, model.PaymentReconcileActionRefunded},
			{disputed, model.PaymentReconcileActionDisputed},
		} {
			rows := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, check.action, check.order.StripeSessionId)
			connect.AssertEqual(t, len(rows), 1)
			connect.AssertEqual(t, rows[0].DryRun, false)
			connect.AssertEqual(t, rows[0].Details["leg"], stripeGiftCheckoutLegName)
			connect.AssertEqual(t, rows[0].Details["gift_order"], check.order.GiftOrderId.String())
			connect.AssertEqual(t, rows[0].Details["charge"], "ch_"+check.order.StripeSessionId)
			connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCredited, check.order.StripeSessionId), 0)
		}
		for _, order := range []*model.GiftOrder{refunded, partial, disputed, pending, unexpanded} {
			connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPending)
			connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 0)
		}
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultRefunded)-refundedBefore, float64(2))
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultDisputed)-disputedBefore, float64(1))

		// recorded: not read again; the errors are
		result = giftReconcileTestRun(t, ctx, false)
		for _, order := range []*model.GiftOrder{refunded, partial, disputed} {
			connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 2)
		}
		for _, order := range []*model.GiftOrder{pending, unexpanded} {
			connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 3)
		}
	})
}

// A dry run reads for real and fulfils nothing: a paid session is a
// would_credit dry-run row, and the next real run fulfils it.
func TestPaymentReconcileGiftCheckoutDryRunFulfilsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)

		order := giftReconcileTestOrder(t, ctx, "cs_giftrec_dry", 1, time.Hour)
		env.setCheckoutSession(order.StripeSessionId, giftReconcileTestSession(order, "complete", "paid"))
		fulfilledBefore := giftReconcileTestCount(giftCheckoutReconcileResultFulfilled)

		result := giftReconcileTestRun(t, ctx, true)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 1)
		rows := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionWouldCredit, order.StripeSessionId)
		connect.AssertEqual(t, len(rows), 1)
		connect.AssertEqual(t, rows[0].DryRun, true)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, order.GiftOrderId), 0)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultFulfilled)-fulfilledBefore, float64(0))

		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultFulfilled)-fulfilledBefore, float64(1))
	})
}

// The one payment a gift order is fulfilled on: its total, in usd, in any
// case of the currency code.
func TestGiftCheckoutPaysTotal(t *testing.T) {
	session := func(amount int, currency string) *StripeEventCheckoutCompleteObject {
		return &StripeEventCheckoutCompleteObject{AmountTotal: amount, Currency: currency}
	}
	connect.AssertEqual(t, giftCheckoutPaysTotal(session(3999, "usd"), 3999), true)
	connect.AssertEqual(t, giftCheckoutPaysTotal(session(3999, "USD"), 3999), true)
	connect.AssertEqual(t, giftCheckoutPaysTotal(session(3998, "usd"), 3999), false)
	connect.AssertEqual(t, giftCheckoutPaysTotal(session(3999, "eur"), 3999), false)
	connect.AssertEqual(t, giftCheckoutPaysTotal(session(3999, ""), 3999), false)
}

// A session Stripe does not know is the order's error (never a store error,
// so the stripe watermark advances) until it has persisted through the
// bounded retry; then it is recorded once as unknown_to_store and is no
// longer an error. It is still read every run, so a later answer is
// reconciled as usual.
func TestPaymentReconcileGiftCheckoutUnknownSessionBoundedRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		env := newGiftReconcileTest(t)
		previousRetryRuns := giftCheckoutUnknownToStoreRetryRuns
		giftCheckoutUnknownToStoreRetryRuns = 3
		t.Cleanup(func() { giftCheckoutUnknownToStoreRetryRuns = previousRetryRuns })
		unknownBefore := giftReconcileTestCount(giftCheckoutReconcileResultUnknownToStore)

		order := giftReconcileTestOrder(t, ctx, "cs_giftrec_lost", 1, time.Hour)
		for run := 1; run <= 2; run += 1 {
			result := giftReconcileTestRun(t, ctx, false)
			connect.AssertEqual(t, result.Errors, 1)
			connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].Errors, 0)
			connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].GiftCheckoutErrors, 1)
			errors := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionError, order.StripeSessionId)
			connect.AssertEqual(t, len(errors), 1)
			connect.AssertEqual(t, errors[0].Details["reason"], giftCheckoutSessionNotFoundReason)
		}
		// the order's errors never held the invoice listing's watermark
		_, watermarked := model.GetPaymentReconcileWatermark(ctx, model.SubscriptionMarketStripe)
		connect.AssertEqual(t, watermarked, true)

		// the third run spends the last retry: recorded once, not an error
		result := giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.StoreResults[model.SubscriptionMarketStripe].UnknownToStore, 1)
		recorded := giftReconcileTestEvents(ctx, result.RunId, model.SubscriptionMarketStripe, model.PaymentReconcileActionUnknownToStore, order.StripeSessionId)
		connect.AssertEqual(t, len(recorded), 1)
		connect.AssertEqual(t, recorded[0].Details["leg"], stripeGiftCheckoutLegName)
		connect.AssertEqual(t, recorded[0].Details["reason"], giftCheckoutSessionNotFoundReason)
		connect.AssertEqual(t, giftReconcileTestCount(giftCheckoutReconcileResultUnknownToStore)-unknownBefore, float64(1))

		// still read, still not an error, recorded once
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, env.checkoutSessionReadCount(order.StripeSessionId), 4)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionUnknownToStore, order.StripeSessionId), 1)

		// Stripe answers again, paid: fulfilled as usual
		env.setCheckoutSession(order.StripeSessionId, giftReconcileTestSession(order, "complete", "paid"))
		result = giftReconcileTestRun(t, ctx, false)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 1)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, order.GiftOrderId).Status, model.GiftOrderStatusPaid)
	})
}
