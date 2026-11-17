package controller

// Database-backed tests of the daily cap on friend emails
// (gift_rails_controller.go): what counts, the refusal on both purchase
// routes before anything is made, and the second check that catches orders
// stored at the same time. Needs the test database (server.DefaultTestEnv).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Stores an order as the purchase routes do, without their checks.
func giftRailsTestOrder(t testing.TB, ctx context.Context, email string, delivery string, quantity int) *model.GiftOrder {
	t.Helper()
	unitPriceCents := model.Gift().UnitPriceCents(quantity)
	order := &model.GiftOrder{
		Quantity:       quantity,
		UnitPriceCents: unitPriceCents,
		TotalCents:     unitPriceCents * int64(quantity),
		Rail:           model.GiftRailStripe,
		Delivery:       delivery,
		BuyerEmail:     email,
		Language:       "en",
	}
	if delivery == model.GiftDeliveryThem {
		for i := range quantity {
			order.Recipients = append(order.Recipients, fmt.Sprintf("friend%d@example.invalid", i))
		}
	}
	connect.AssertEqual(t, model.CreateGiftOrder(ctx, order), nil)
	return order
}

func giftRailsTestCount(ctx context.Context, sql string) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, sql).Scan(&count))
	})
	return
}

// One buyer, by email case-insensitively, has at most 20 friend emails in 24
// hours: the recipients of their delivery "them" orders of the last 24 hours
// that are pending or paid, this order's included. A refund, an older order,
// cards to the buyer ("me") and another buyer's orders do not count. Over the
// cap, both routes refuse with gift_daily_limit before an order, a Checkout
// session or an invoice is made, and say "me" is not capped. An order stored
// at the same time as another is caught once stored, and removed.
func TestGiftDailyFriendEmailLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		api := newGiftStripeTestApi(t)
		signedOut := giftPurchaseTestSession()
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		since := func() time.Time {
			return server.NowUtc().Add(-model.GiftFriendEmailWindow)
		}

		// 10 pending and 5 paid count; a refund, a day-old order, cards to
		// the buyer and another buyer's do not
		giftRailsTestOrder(t, ctx, "Giver@Example.com", model.GiftDeliveryThem, 10)
		paid := giftRailsTestOrder(t, ctx, "giver@example.COM", model.GiftDeliveryThem, 5)
		cryptoDbExec(ctx, `UPDATE gift_order SET status = $2, paid_time = $3 WHERE gift_order_id = $1`, paid.GiftOrderId, model.GiftOrderStatusPaid, server.NowUtc())
		refunded := giftRailsTestOrder(t, ctx, "giver@example.com", model.GiftDeliveryThem, 4)
		cryptoDbExec(ctx, `UPDATE gift_order SET status = $2 WHERE gift_order_id = $1`, refunded.GiftOrderId, model.GiftOrderStatusRefunded)
		old := giftRailsTestOrder(t, ctx, "giver@example.com", model.GiftDeliveryThem, 3)
		cryptoDbExec(ctx, `UPDATE gift_order SET create_time = $2 WHERE gift_order_id = $1`, old.GiftOrderId, server.NowUtc().Add(-25*time.Hour))
		giftRailsTestOrder(t, ctx, "giver@example.com", model.GiftDeliveryMe, 20)
		giftRailsTestOrder(t, ctx, "someone@example.com", model.GiftDeliveryThem, 10)
		connect.AssertEqual(t, model.GetGiftFriendEmailCount(ctx, "GIVER@example.com", since()), 15)
		connect.AssertEqual(t, model.GetGiftFriendEmailCount(ctx, " giver@example.com ", since()), 15)

		checkout := func(email string, delivery string, quantity int) *GiftCheckoutResult {
			t.Helper()
			giftPurchaseTestResetLimiters()
			args := &GiftCheckoutArgs{
				GiftPurchaseArgs: GiftPurchaseArgs{
					Quantity: quantity,
					Delivery: delivery,
					Email:    email,
					Language: "en",
				},
				UiMode: StripeUiModeHosted,
			}
			if delivery == model.GiftDeliveryThem {
				for i := range quantity {
					args.Recipients = append(args.Recipients, fmt.Sprintf("new%d@example.invalid", i))
				}
			}
			result, err := GiftCheckout(args, signedOut)
			connect.AssertEqual(t, err, nil)
			return result
		}
		orders := func() int {
			return giftRailsTestCount(ctx, `SELECT count(*) FROM gift_order`)
		}

		// 15 and 6 more is over: refused before an order or a session
		ordersBefore := orders()
		refused := checkout("giver@EXAMPLE.com", model.GiftDeliveryThem, 6)
		connect.AssertEqual(t, refused.Error != nil, true)
		connect.AssertEqual(t, refused.Error.Code, GiftErrorCodeDailyLimit)
		connect.AssertEqual(t, strings.Contains(refused.Error.Message, `("me")`), true)
		connect.AssertEqual(t, strings.Contains(refused.Error.Message, "20"), true)
		connect.AssertEqual(t, refused.OrderId, "")
		connect.AssertEqual(t, orders(), ordersBefore)
		connect.AssertEqual(t, api.sessionCount(), 0)

		// 15 and 5 is the cap: accepted
		accepted := checkout("giver@example.com", model.GiftDeliveryThem, 5)
		connect.AssertEqual(t, accepted.Error == nil, true)
		connect.AssertEqual(t, accepted.Url != "", true)
		connect.AssertEqual(t, model.GetGiftFriendEmailCount(ctx, "giver@example.com", since()), 20)

		// at the cap one more friend is refused, and cards to the buyer are
		// not capped
		connect.AssertEqual(t, checkout("Giver@example.com", model.GiftDeliveryThem, 1).Error.Code, GiftErrorCodeDailyLimit)
		toMe := checkout("giver@example.com", model.GiftDeliveryMe, 20)
		connect.AssertEqual(t, toMe.Error == nil, true)
		// another buyer has their own cap
		connect.AssertEqual(t, checkout("someone@example.com", model.GiftDeliveryThem, 10).Error == nil, true)
		connect.AssertEqual(t, checkout("someone@example.com", model.GiftDeliveryThem, 1).Error.Code, GiftErrorCodeDailyLimit)
		connect.AssertEqual(t, api.sessionCount(), 3)

		// the coin rail refuses the same way, before an invoice is made
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetZec)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetZec: wallet})
		_, err := RunCryptoAddressRefill(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		resetCryptoAvailability()
		giftPurchaseTestResetLimiters()
		intentsBefore := giftRailsTestCount(ctx, `SELECT count(*) FROM crypto_payment_intent`)
		ordersBefore = orders()
		coin, err := GiftCryptoIntent(&GiftCryptoIntentArgs{
			GiftPurchaseArgs: GiftPurchaseArgs{
				Quantity:   2,
				Delivery:   model.GiftDeliveryThem,
				Email:      "GIVER@example.com",
				Recipients: []string{"c1@example.invalid", "c2@example.invalid"},
				Language:   "en",
			},
			Asset: model.CryptoAssetZec,
		}, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, coin.Error != nil, true)
		connect.AssertEqual(t, coin.Error.Code, GiftErrorCodeDailyLimit)
		connect.AssertEqual(t, coin.OrderId, "")
		connect.AssertEqual(t, giftRailsTestCount(ctx, `SELECT count(*) FROM crypto_payment_intent`), intentsBefore)
		connect.AssertEqual(t, orders(), ordersBefore)

		// two orders that each passed the first check, stored at the same
		// time: once stored, the one past the cap is removed and refused
		giftConfig := model.Gift()
		now := server.NowUtc()
		giftRailsTestOrder(t, ctx, "pair@example.com", model.GiftDeliveryThem, 15)
		first := giftRailsTestOrder(t, ctx, "pair@example.com", model.GiftDeliveryThem, 5)
		second := giftRailsTestOrder(t, ctx, "Pair@example.com", model.GiftDeliveryThem, 3)
		code, message := giftKeepFriendEmailLimit(ctx, second, giftConfig, now)
		connect.AssertEqual(t, code, GiftErrorCodeDailyLimit)
		connect.AssertEqual(t, message != "", true)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, second.GiftOrderId), (*model.GiftOrder)(nil))
		connect.AssertEqual(t, giftRailsTestCount(ctx, fmt.Sprintf(`SELECT count(*) FROM gift_order_recipient WHERE gift_order_id = '%s'`, second.GiftOrderId)), 0)
		// the other, now at the cap, stands
		code, message = giftKeepFriendEmailLimit(ctx, first, giftConfig, now)
		connect.AssertEqual(t, code, "")
		connect.AssertEqual(t, message, "")
		connect.AssertEqual(t, model.GetGiftOrder(ctx, first.GiftOrderId) != nil, true)
		// an order already started is never removed
		connect.AssertEqual(t, model.SetGiftOrderStripeSessionId(ctx, first.GiftOrderId, "cs_test_gift_rails_started"), nil)
		connect.AssertEqual(t, model.RemoveUnstartedGiftOrder(ctx, first.GiftOrderId), false)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, first.GiftOrderId) != nil, true)
	})
}
