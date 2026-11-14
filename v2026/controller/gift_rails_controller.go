package controller

// The daily cap on friend emails (mmm/GIFTOFPRIVACY.md §9.2, §10.2): one buyer,
// by their email case-insensitively, has at most model.GiftFriendEmailDailyLimit
// friend emails in any 24 hours, counted as the recipients of their delivery
// "them" orders made in the last 24 hours that are pending or paid, this
// order's included. Cards delivered to the buyer ("me") are not capped beyond
// gift.max_quantity per order.
//
// An order is stored in its own transaction (model.CreateGiftOrder), so the cap
// is checked twice: before anything is made, which refuses the usual case
// with no order, invoice or Checkout session; and again once the order is
// stored, counting every order of the buyer's, the ones stored at the same
// time included. An order over the cap then is removed before it is started.
// Two orders that cross the cap together can both be refused; neither is ever
// both accepted. No lock is held across the two transactions, so this works
// through any connection pooler.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026/model"
)

// giftFriendEmailLimitRefusal checks a new order against the buyer's daily
// friend emails before it is stored: its code and message when the order's
// recipients would take the buyer past the cap, else "".
func giftFriendEmailLimitRefusal(
	ctx context.Context,
	order *model.GiftOrder,
	giftConfig *model.GiftConfig,
	now time.Time,
) (errCode string, errMessage string) {
	if order.Delivery != model.GiftDeliveryThem {
		return "", ""
	}
	count := model.GetGiftFriendEmailCount(ctx, order.BuyerEmail, now.Add(-model.GiftFriendEmailWindow))
	if model.GiftFriendEmailDailyLimit < count+len(order.Recipients) {
		return GiftErrorCodeDailyLimit, giftDailyLimitMessage(giftConfig)
	}
	return "", ""
}

// giftKeepFriendEmailLimit checks a stored order against the buyer's daily
// friend emails again, counting every order of the buyer's, this one and any
// stored at the same time. An order over the cap is removed, never started,
// and refused with its code and message; else "".
func giftKeepFriendEmailLimit(
	ctx context.Context,
	order *model.GiftOrder,
	giftConfig *model.GiftConfig,
	now time.Time,
) (errCode string, errMessage string) {
	if order.Delivery != model.GiftDeliveryThem {
		return "", ""
	}
	count := model.GetGiftFriendEmailCount(ctx, order.BuyerEmail, now.Add(-model.GiftFriendEmailWindow))
	if count <= model.GiftFriendEmailDailyLimit {
		return "", ""
	}
	if !model.RemoveUnstartedGiftOrder(ctx, order.GiftOrderId) {
		// never started, so nobody can pay it; it stays pending and counts
		// against the buyer for its 24 hours
		glog.Errorf("[gift]gift order %s went past the daily friend emails and could not be removed\n", order.GiftOrderId)
	}
	glog.Infof("[gift]gift order %s refused at the daily friend emails: %d\n", order.GiftOrderId, count)
	return GiftErrorCodeDailyLimit, giftDailyLimitMessage(giftConfig)
}

// What a buyer at the daily cap reads: the cap, and that cards delivered to
// them have none beyond the per-order quantity.
func giftDailyLimitMessage(giftConfig *model.GiftConfig) string {
	return fmt.Sprintf(
		"One email address can send gift cards to at most %d friends a day. You can still have the cards delivered to you (\"me\") and pass them on yourself: that has no daily limit beyond %d cards per order.",
		model.GiftFriendEmailDailyLimit,
		max(giftConfig.MaxQuantity, 1),
	)
}
