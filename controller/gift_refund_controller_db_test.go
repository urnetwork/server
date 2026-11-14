package controller

// Database-backed tests of a card gift order's refunds and disputes as the
// Stripe webhook meets them (gift_refund_controller.go,
// model/gift_refund_model.go; decision 16), against the fake Stripe API of
// subscription_stripe_refund_test.go. Needs the test database
// (server.DefaultTestEnv).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A card order paid through its checkout session, and the charge that paid
// it, which the fake Stripe API serves with the session its payment intent
// finds.
type giftRefundTestOrder struct {
	order           *model.GiftOrder
	codes           []*model.GiftCode
	sessionId       string
	chargeId        string
	paymentIntentId string
}

func giftRefundTestPaidOrder(
	t testing.TB,
	ctx context.Context,
	env *stripeRefundTestEnv,
	quantity int,
	delivery string,
	sendOn time.Time,
) *giftRefundTestOrder {
	t.Helper()
	order, sessionId := giftDeliveryTestPendingOrder(t, ctx, quantity, delivery, sendOn)
	codes := giftDeliveryTestFulfil(t, ctx, order.GiftOrderId, sessionId, server.NowUtc())
	suffix := server.NewId().String()
	paid := &giftRefundTestOrder{
		order:           model.GetGiftOrder(ctx, order.GiftOrderId),
		codes:           codes,
		sessionId:       sessionId,
		chargeId:        "ch_gift_" + suffix,
		paymentIntentId: "pi_gift_" + suffix,
	}
	env.charges[paid.chargeId] = map[string]any{
		"id":             paid.chargeId,
		"payment_intent": paid.paymentIntentId,
		"amount":         paid.order.TotalCents,
	}
	env.sessionsByPaymentIntent[paid.paymentIntentId] = []map[string]any{{"id": sessionId}}
	return paid
}

// Delivers one webhook event, which must answer 200.
func giftRefundTestDeliver(t testing.TB, ctx context.Context, eventType string, object map[string]any) {
	t.Helper()
	_, err := StripeWebhook(stripeWebhookEvent(t, eventType, object), reconcileTestSession(t, ctx))
	connect.AssertEqual(t, err, nil)
}

// A refund of the order's charge, as refund.created delivers it.
func (self *giftRefundTestOrder) refund(t testing.TB, ctx context.Context, refundId string, amount int) {
	t.Helper()
	giftRefundTestDeliver(t, ctx, "refund.created", map[string]any{
		"id":     refundId,
		"charge": self.chargeId,
		"amount": amount,
	})
}

// A dispute event of the order's charge.
func (self *giftRefundTestOrder) dispute(t testing.TB, ctx context.Context, eventType string, disputeId string, status string) {
	t.Helper()
	giftRefundTestDeliver(t, ctx, eventType, map[string]any{
		"id":     disputeId,
		"charge": self.chargeId,
		"amount": self.order.TotalCents,
		"status": status,
	})
}

// The reconciliation events of one store object.
func giftRefundTestEvents(t testing.TB, ctx context.Context, action string, evidence string) int {
	return countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, action, evidence)
}

// Refunds void the order's unredeemed codes by the amount refunded: ⌊the
// cumulative refunded amount / the unit price⌋ codes, the highest first,
// partial refunds included, each refund counted once however it is delivered.
// A redeemed code's year never ends. More refunded than the unredeemed codes
// are worth voids them all, refunds the order, and is reported for an
// operator.
func TestGiftStripeRefundsVoidUnredeemedCodes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		env := newStripeRefundTestEnv(t)
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		paid := giftRefundTestPaidOrder(t, ctx, env, 5, model.GiftDeliveryMe, time.Time{})
		giftOrderId := paid.order.GiftOrderId
		unit := int(paid.order.UnitPriceCents)
		connect.AssertEqual(t, unit, 2399)
		friendNetworkId := giftCryptoTestRedeem(t, ctx, paid.codes[0].Secret)

		// less than one code's price voids nothing; the order stays paid with
		// its refunded total
		paid.refund(t, ctx, "re_gift_1", unit-1)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, giftOrderId), []bool{false, false, false, false, false})
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, model.GetGiftOrderRefundedCents(ctx, giftOrderId), int64(unit-1))
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionRefunded, "re_gift_1"), 1)

		// a cent more makes one code's worth: the highest code is void
		paid.refund(t, ctx, "re_gift_2", 1)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, giftOrderId), []bool{false, false, false, false, true})
		connect.AssertEqual(t, model.GetGiftOrderRefundedCents(ctx, giftOrderId), int64(unit))

		// the same refunds again, as charge.refunded lists them, and once
		// more as refund.created: nothing more
		giftRefundTestDeliver(t, ctx, "charge.refunded", map[string]any{
			"id":              paid.chargeId,
			"payment_intent":  paid.paymentIntentId,
			"amount":          paid.order.TotalCents,
			"amount_refunded": unit,
			"refunds": map[string]any{"data": []map[string]any{
				{"id": "re_gift_1", "charge": paid.chargeId, "amount": unit - 1},
				{"id": "re_gift_2", "charge": paid.chargeId, "amount": 1},
			}},
		})
		paid.refund(t, ctx, "re_gift_2", 1)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, giftOrderId), []bool{false, false, false, false, true})
		connect.AssertEqual(t, model.GetGiftOrderRefundedCents(ctx, giftOrderId), int64(unit))
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionRefunded, "re_gift_2"), 1)

		// two codes' worth more: the next two
		paid.refund(t, ctx, "re_gift_3", 2*unit)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, giftOrderId), []bool{false, false, true, true, true})
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusPaid)

		// more than the unredeemed codes are worth: the last one is void, the
		// order is refunded, and the rest is reported for an operator
		paid.refund(t, ctx, "re_gift_4", 2*unit)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, giftOrderId), []bool{false, true, true, true, true})
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusRefunded)
		connect.AssertEqual(t, model.GetGiftOrderRefundedCents(ctx, giftOrderId), int64(5*unit))
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionRefundUnmatched, "re_gift_4"), 1)
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionRefunded, "re_gift_4"), 0)

		// the redeemed code's year continues
		codes := model.GetGiftOrderCodes(ctx, giftOrderId)
		connect.AssertEqual(t, codes[0].Redeemed, true)
		connect.AssertEqual(t, codes[0].Voided, false)
		connect.AssertEqual(t, model.IsPro(ctx, &friendNetworkId), true)
		checked, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: codes[1].Secret}, taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusVoided)
	})
}

// A dispute voids every unredeemed code when it opens, and a redeemed code's
// year continues. Lost, the redeemed codes' years end and the order is
// refunded. Won, exactly what the dispute voided and ended comes back, never a
// refund's voids, and the order is paid again; a friend's email withdrawn
// while its code was void is owed again, once, and one sent before is not.
func TestGiftStripeDisputes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		env := newStripeRefundTestEnv(t)
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		// an order whose friends get their cards on a date: its first friend
		// redeems, then a refund of one code, then a dispute
		sendOn := giftDeliveryTestDate(40)
		friendDue := giftDeliveryTestChicagoMorning(t, sendOn)
		won := giftRefundTestPaidOrder(t, ctx, env, 4, model.GiftDeliveryThem, sendOn)
		wonId := won.order.GiftOrderId
		giftDeliveryTestPost(t, ctx, wonId)
		wonFriendId := giftCryptoTestRedeem(t, ctx, won.codes[0].Secret)
		giftRetentionTestSent(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(wonId, 1), server.NowUtc())
		won.refund(t, ctx, "re_gift_disputed", int(won.order.UnitPriceCents))
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, wonId), []bool{false, false, false, true})

		// an inquiry takes no money and voids nothing
		won.dispute(t, ctx, "charge.dispute.created", "dp_gift_inquiry", "warning_needs_response")
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, wonId), []bool{false, false, false, true})

		// opened: every unredeemed code is void, once however it arrives
		won.dispute(t, ctx, "charge.dispute.created", "dp_gift_won", "needs_response")
		won.dispute(t, ctx, "charge.dispute.funds_withdrawn", "dp_gift_won", "needs_response")
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, wonId), []bool{false, true, true, true})
		connect.AssertEqual(t, model.GetGiftOrder(ctx, wonId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, model.IsPro(ctx, &wonFriendId), true)
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionDisputed, "dp_gift_won"), 1)

		// the friends' date comes while the codes are void: the third
		// friend's email is withdrawn, and the fourth's, whose code was
		// refunded (the second's went out before)
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, friendDue.Add(time.Minute))
		connect.AssertEqual(t, giftCryptoTestFriendSends(sender), map[int][]string{0: {giftCardCodeGrouped(won.codes[0].Secret)}})
		connect.AssertEqual(t, giftCryptoTestOwedFriendEmails(t, ctx, wonId), []int{})

		// won: the dispute's codes come back, the refund's does not, and the
		// third friend's email is owed again, once
		for range 2 {
			won.dispute(t, ctx, "charge.dispute.closed", "dp_gift_won", "won")
		}
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, wonId), []bool{false, false, false, true})
		connect.AssertEqual(t, model.GetGiftOrder(ctx, wonId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, model.IsPro(ctx, &wonFriendId), true)
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionDisputeWon, "dp_gift_won"), 1)
		connect.AssertEqual(t, giftCryptoTestOwedFriendEmails(t, ctx, wonId), []int{2})
		deliverAccountMessagesAt(ctx, sender, friendDue.Add(2*time.Minute))
		connect.AssertEqual(t, giftCryptoTestFriendSends(sender), map[int][]string{
			0: {giftCardCodeGrouped(won.codes[0].Secret)},
			2: {giftCardCodeGrouped(won.codes[2].Secret)},
		})
		checked, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: won.codes[1].Secret}, taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusOk)

		// an order whose dispute is lost: its redeemed codes' years end too,
		// both in one transaction, and the order is refunded
		lost := giftRefundTestPaidOrder(t, ctx, env, 3, model.GiftDeliveryMe, time.Time{})
		lostId := lost.order.GiftOrderId
		lostFriendIds := []server.Id{
			giftCryptoTestRedeem(t, ctx, lost.codes[0].Secret),
			giftCryptoTestRedeem(t, ctx, lost.codes[1].Secret),
		}
		lost.dispute(t, ctx, "charge.dispute.created", "dp_gift_lost", "needs_response")
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, lostId), []bool{false, false, true})
		for _, lostFriendId := range lostFriendIds {
			connect.AssertEqual(t, model.IsPro(ctx, &lostFriendId), true)
		}
		// the closed event names its payment intent; a redelivery changes
		// nothing
		for range 2 {
			giftRefundTestDeliver(t, ctx, "charge.dispute.closed", map[string]any{
				"id":             "dp_gift_lost",
				"charge":         lost.chargeId,
				"payment_intent": lost.paymentIntentId,
				"amount":         lost.order.TotalCents,
				"status":         "lost",
			})
		}
		for _, lostFriendId := range lostFriendIds {
			connect.AssertEqual(t, model.IsPro(ctx, &lostFriendId), false)
		}
		connect.AssertEqual(t, model.GetGiftOrder(ctx, lostId).Status, model.GiftOrderStatusRefunded)
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionDisputed, "dp_gift_lost"), 1)
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionRevoked, "dp_gift_lost"), 1)

		// were it won after all, the years it ended and the code it voided
		// come back, under one admission, and the order is paid again
		lost.dispute(t, ctx, "charge.dispute.closed", "dp_gift_lost", "won")
		for _, lostFriendId := range lostFriendIds {
			connect.AssertEqual(t, model.IsPro(ctx, &lostFriendId), true)
		}
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, lostId), []bool{false, false, false})
		connect.AssertEqual(t, model.GetGiftOrder(ctx, lostId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, giftRefundTestEvents(t, ctx, model.PaymentReconcileActionDisputeWon, "dp_gift_lost"), 1)
	})
}
