package controller

// Refunds and disputes of a gift order's card payment, as the Stripe webhook
// meets them (mmm/GIFTOFPRIVACY.md §5, decision 16; model/gift_refund_model.go):
//
//   - refund.created and charge.refunded on a gift order's charge
//     (stripeHandleRefund): the order's unredeemed codes are voided by the
//     amount refunded, ⌊cumulative refunded / unit price⌋ in all, the highest
//     code first, partial refunds included; a redeemed code's year never
//     ends. A refunded total worth more than the unredeemed codes is recorded
//     as refund_unmatched, which the payment monitor reports, naming the order;
//   - charge.dispute.created and charge.dispute.funds_withdrawn: every
//     unredeemed code is voided at once; redeemed years continue;
//   - charge.dispute.closed lost: the redeemed codes' years end too, and the
//     order is refunded (event revoked: the dispute was counted disputed when
//     it opened);
//   - charge.dispute.closed won: exactly the codes the dispute voided, and the
//     years it ended, come back, and the order is paid again.
//
// A friend's email that came due while its code was void was withdrawn by the
// delivery (GiftCardFriendTemplate). When the code comes back (a won dispute,
// or a crypto payment credited again), that email is owed again under a new
// key (giftReoweFriendEmailsInTx).

import (
	"context"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// stripeGiftOrderIdForPaymentIntent is the gift order a payment intent's
// checkout session paid for, nil when it paid for none.
func stripeGiftOrderIdForPaymentIntent(ctx context.Context, paymentIntentId string) (*server.Id, error) {
	if paymentIntentId == "" {
		return nil, nil
	}
	sessionId, err := stripeCheckoutSessionIdForPaymentIntent(ctx, paymentIntentId)
	if err != nil || sessionId == "" {
		return nil, err
	}
	return model.GetGiftOrderIdForStripeSession(ctx, sessionId), nil
}

// stripeGiftRefundInTx applies one refund of a gift order's charge (its id and
// amount), or the opening of a dispute (action disputed; its id), in
// stripeHandleRefund's transaction, writing what it did into the event's
// details. Returns the event's action: refund_unmatched when more was
// refunded than the unredeemed codes were worth.
func stripeGiftRefundInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	refundId string,
	action string,
	amount int,
	details map[string]any,
	now time.Time,
) string {
	details["gift_order"] = giftOrderId.String()
	if action == model.PaymentReconcileActionDisputed {
		voided := model.DisputeGiftOrderCodesInTx(tx, ctx, giftOrderId, refundId, now)
		details["voided_codes"] = len(voided)
		return action
	}
	outcome := model.RefundGiftOrderCodesInTx(tx, ctx, giftOrderId, refundId, int64(amount), now)
	if outcome == nil {
		details["reason"] = "gift_order_not_found"
		return model.PaymentReconcileActionRefundUnmatched
	}
	details["voided_codes"] = len(outcome.VoidedCodeIds)
	details["refunded_cents"] = outcome.RefundedCents
	details["order_refunded"] = outcome.OrderRefunded
	if outcome.Unfulfilled {
		details["unfulfilled"] = true
	}
	if outcome.Exceeds {
		// every unredeemed code is void, and what was refunded beyond them is
		// for an operator
		details["reason"] = "refund_exceeds_unredeemed_codes"
		glog.Errorf("[gift]refund %s of gift order %s is worth more than its unredeemed codes; every one is void\n", refundId, giftOrderId)
		return model.PaymentReconcileActionRefundUnmatched
	}
	return action
}

// stripeHandleGiftDisputeClosed is charge.dispute.closed for a gift order's
// charge, won or lost, once per dispute and outcome (the stripe_refund ledger,
// keyed won/<dispute> or lost/<dispute>), with its event (dispute_won, or
// revoked for the years a loss ended), in one transaction; a failure answers
// non-2xx so Stripe delivers it again.
func stripeHandleGiftDisputeClosed(
	dispute *StripeEventDisputeObject,
	chargeId string,
	giftOrderId server.Id,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	won := dispute.Status == stripeDisputeStatusWon
	ledgerId := "lost/" + dispute.Id
	action := model.PaymentReconcileActionRevoked
	if won {
		ledgerId = "won/" + dispute.Id
		action = model.PaymentReconcileActionDisputeWon
	}

	handled := false
	var networkIds []server.Id
	server.Tx(ctx, func(tx server.PgTx) {
		handled = false
		networkIds = nil
		ledgerTag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO stripe_refund (refund_id, charge_id, action)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING
			`,
			ledgerId,
			chargeId,
			action,
		))
		if ledgerTag.RowsAffected() == 0 {
			glog.Infof("[gift]dispute %s closed %s already processed; ignoring redelivery\n", dispute.Id, dispute.Status)
			return
		}
		handled = true

		details := map[string]any{
			"charge":     chargeId,
			"event_type": "charge.dispute.closed",
			"status":     dispute.Status,
			"gift_order": giftOrderId.String(),
		}
		if won {
			if restoration := model.WinGiftOrderDisputeInTx(tx, ctx, giftOrderId, dispute.Id); restoration != nil {
				restoredCodes, restoredYears := giftRestorationCounts(restoration)
				details["restored_codes"] = restoredCodes
				details["restored_years"] = restoredYears
				details["reowed_emails"] = giftReoweFriendEmailsInTx(ctx, tx, restoration, "dispute/"+dispute.Id, now)
				networkIds = restoration.NetworkIds
			}
		} else {
			details["reason"] = "dispute_lost"
			if loss := model.LoseGiftOrderDisputeInTx(tx, ctx, giftOrderId, dispute.Id, now); loss != nil {
				details["voided_codes"] = len(loss.VoidedCodeIds)
				details["ended_years"] = len(loss.EndedCodeIds)
				networkIds = loss.EndedNetworkIds
			}
		}
		server.Raise(model.AddPaymentReconciliationEventInTx(tx, ctx, &model.PaymentReconciliationEvent{
			RunId:    server.NewId(),
			Store:    model.SubscriptionMarketStripe,
			Action:   action,
			Evidence: dispute.Id,
			Details:  details,
		}))
	}, server.TxReadCommitted, server.OptNoRetry())

	if handled {
		for _, networkId := range networkIds {
			model.UpdateProNetwork(ctx, networkId)
		}
		glog.Infof("[gift]dispute %s of gift order %s closed %s\n", dispute.Id, giftOrderId, dispute.Status)
	}
	return &StripeWebhookResult{}, nil
}

// The unredeemed codes a restoration made good again, and the redeemed codes'
// years it put back.
func giftRestorationCounts(restoration *model.GiftRestoration) (codes int, years int) {
	for _, code := range restoration.Codes {
		if code.Year {
			years += 1
		} else {
			codes += 1
		}
	}
	return
}

// giftReoweFriendEmailsInTx owes again, in the transaction that restored
// them, the friends' emails of restored codes that came due while the code was
// void: the delivery withdrew each when it was due. Each is owed under a new
// key, the original's with "/restored/<restoreKey>", due now. The recipient's
// latest email decides, under whichever key owed it (one owed again after an
// earlier restoration included): one sent, or not due yet, is left to the
// outbox. The outbox removes a finished message about a week after it was
// added; for a recipient with none left, the first due time decides, once the
// order's cards were rendered (before that no friend's email was owed yet).
// Returns how many it owed.
func giftReoweFriendEmailsInTx(
	ctx context.Context,
	tx server.PgTx,
	restoration *model.GiftRestoration,
	restoreKey string,
	now time.Time,
) (reowed int) {
	if restoration == nil || restoration.Order == nil || restoration.Order.Delivery != model.GiftDeliveryThem {
		return 0
	}
	order := restoration.Order
	states := model.GetGiftFriendEmailStatesInTx(tx, ctx, order.GiftOrderId)
	for _, code := range restoration.Codes {
		// a redeemed code's friend has their code
		if code.Year || code.N < 0 || len(order.Recipients) <= code.N {
			continue
		}
		recipient := strings.TrimSpace(order.Recipients[code.N])
		if recipient == "" {
			continue
		}
		withdrawn := false
		if state := states[code.N]; state != nil {
			withdrawn = state.SentTime == nil && state.AbandonTime != nil && !state.AbandonTime.Before(code.VoidTime)
		} else if order.PdfReady {
			due := giftFriendDueTime(order)
			withdrawn = !due.Before(code.VoidTime) && !now.Before(due)
		}
		if !withdrawn {
			continue
		}
		if addAccountMessageInTx(ctx, tx, &accountMessage{
			key:      giftFriendEmailKey(order.GiftOrderId, code.N) + "/restored/" + restoreKey,
			userAuth: recipient,
			template: &GiftCardFriendTemplate{
				GiftOrderId:    order.GiftOrderId,
				RecipientIndex: code.N,
			},
		}) {
			reowed += 1
		}
	}
	return
}

// When an order's friends' emails were first due, as the delivery owed them
// once it was paid (giftFriendDeliverTime): 9:00 on its send date in the
// buyer's zone, or when it was paid when that was later or it has no send
// date.
func giftFriendDueTime(order *model.GiftOrder) time.Time {
	paidTime := order.CreateTime
	if order.PaidTime != nil {
		paidTime = *order.PaidTime
	}
	return giftFriendDeliverTime(order, paidTime)
}
