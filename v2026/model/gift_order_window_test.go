package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The whole printed day is redeemable in every zone, from UTC-12 to UTC+14 by
// quarter hours: the day ends no later than the cutoff anywhere.
func giftRedeemDayIsWhole(t *testing.T, name string, redeemBy time.Time) {
	t.Helper()
	date := GiftRedeemByDate(redeemBy)
	for offset := -12 * 60; offset <= 14*60; offset += 15 {
		zone := time.FixedZone("", offset*60)
		end := time.Date(date.Year(), date.Month(), date.Day()+1, 0, 0, 0, 0, zone)
		if redeemBy.Before(end) {
			t.Fatalf("%s: %s ends at %s at UTC%+d min, after the cutoff %s", name, date.Format(time.DateOnly), end.UTC(), offset, redeemBy)
		}
	}
	// and the next day is not: the date printed is the last one
	next := time.Date(date.Year(), date.Month(), date.Day()+2, 0, 0, 0, 0, time.FixedZone("", -12*60*60))
	if !redeemBy.Before(next) {
		t.Fatalf("%s: the day after %s is also whole before %s", name, date.Format(time.DateOnly), redeemBy)
	}
}

// The reviewer's cases: a scheduled friend gets a full year from the card's
// arrival (9:00 on the buyer's date in the buyer's zone), whatever lies between
// the payment and the delivery (a 29 February, a whole year), with an
// inclusive printed date; delivery "me" keeps the window from the payment.
func TestGiftRedeemWindowFromDelivery(t *testing.T) {
	year := 8760 * time.Hour
	date := func(year int, month time.Month, day int) time.Time {
		return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	}
	cases := []struct {
		name      string
		zone      string
		paidTime  time.Time
		sendOn    *time.Time
		arrival   time.Time
		printed   time.Time
		cutoffUtc time.Time
	}{
		{
			name:      "paid 2026-11-13 in Los Angeles, sent 2027-11-13",
			zone:      "America/Los_Angeles",
			paidTime:  time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC),
			sendOn:    ptrTime(date(2027, 11, 13)),
			arrival:   time.Date(2027, 11, 13, 17, 0, 0, 0, time.UTC),
			printed:   date(2028, 11, 13),
			cutoffUtc: time.Date(2028, 11, 14, 12, 0, 0, 0, time.UTC),
		},
		{
			name:      "paid 2027-06-01 in Los Angeles, sent 2028-06-01 (across 29 February 2028)",
			zone:      "America/Los_Angeles",
			paidTime:  time.Date(2027, 6, 1, 19, 0, 0, 0, time.UTC),
			sendOn:    ptrTime(date(2028, 6, 1)),
			arrival:   time.Date(2028, 6, 1, 16, 0, 0, 0, time.UTC),
			printed:   date(2029, 6, 1),
			cutoffUtc: time.Date(2029, 6, 2, 12, 0, 0, 0, time.UTC),
		},
		{
			name:      "paid 2027-06-01 in Tokyo, sent 2028-06-02",
			zone:      "Asia/Tokyo",
			paidTime:  time.Date(2027, 6, 1, 3, 0, 0, 0, time.UTC),
			sendOn:    ptrTime(date(2028, 6, 2)),
			arrival:   time.Date(2028, 6, 2, 0, 0, 0, 0, time.UTC),
			printed:   date(2029, 6, 2),
			cutoffUtc: time.Date(2029, 6, 3, 12, 0, 0, 0, time.UTC),
		},
		{
			name:      "delivery me, paid 2026-11-13 10:00 in Los Angeles",
			zone:      "America/Los_Angeles",
			paidTime:  time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC),
			arrival:   time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC),
			printed:   date(2027, 11, 13),
			cutoffUtc: time.Date(2027, 11, 14, 12, 0, 0, 0, time.UTC),
		},
		{
			name:      "delivery me, paid 2027-06-01 in Los Angeles (a year across 29 February 2028)",
			zone:      "America/Los_Angeles",
			paidTime:  time.Date(2027, 6, 1, 19, 0, 0, 0, time.UTC),
			arrival:   time.Date(2027, 6, 1, 19, 0, 0, 0, time.UTC),
			printed:   date(2028, 6, 1),
			cutoffUtc: time.Date(2028, 6, 2, 12, 0, 0, 0, time.UTC),
		},
		{
			name:      "delivery me, paid late on 2026-11-13 in Tokyo (still the 13th there)",
			zone:      "Asia/Tokyo",
			paidTime:  time.Date(2026, 11, 13, 14, 0, 0, 0, time.UTC),
			arrival:   time.Date(2026, 11, 13, 14, 0, 0, 0, time.UTC),
			printed:   date(2027, 11, 13),
			cutoffUtc: time.Date(2027, 11, 14, 12, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		windowStart := c.paidTime
		if c.sendOn != nil {
			windowStart = GiftOrderDeliverTime(*c.sendOn, c.zone)
		}
		if !windowStart.Equal(c.arrival) {
			t.Fatalf("%s: the card arrives at %s, want %s", c.name, windowStart, c.arrival)
		}
		redeemBy := GiftRedeemBy(windowStart, year, GiftOrderLocation(c.zone))
		if !redeemBy.Equal(c.cutoffUtc) || redeemBy.Location() != time.UTC {
			t.Fatalf("%s: redeem by %s, want %s", c.name, redeemBy, c.cutoffUtc)
		}
		if printed := GiftRedeemByDate(redeemBy); !printed.Equal(c.printed) {
			t.Fatalf("%s: prints %s, want %s", c.name, printed.Format(time.DateOnly), c.printed.Format(time.DateOnly))
		}
		// at least a full year from arrival, never less than redeemable_for
		if redeemBy.Sub(windowStart) < year || redeemBy.Before(windowStart.In(GiftOrderLocation(c.zone)).AddDate(1, 0, 0)) {
			t.Fatalf("%s: the window %s is less than a year", c.name, redeemBy.Sub(windowStart))
		}
		giftRedeemDayIsWhole(t, c.name, redeemBy)
	}

	// a window that is not whole years is its duration, then the end of its
	// last day everywhere
	start := time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)
	redeemBy := GiftRedeemBy(start, 90*24*time.Hour, GiftOrderLocation("Europe/Berlin"))
	if !redeemBy.Equal(time.Date(2027, 2, 12, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("90 days: redeem by %s", redeemBy)
	}
	giftRedeemDayIsWhole(t, "90 days", redeemBy)

	// any cutoff prints its last whole day: one at 00:00 UTC prints the day
	// before the day before
	for _, cutoff := range []time.Time{
		time.Date(2027, 11, 14, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 11, 14, 11, 59, 0, 0, time.UTC),
		time.Date(2027, 11, 14, 12, 0, 0, 0, time.UTC),
		time.Date(2027, 11, 15, 3, 0, 0, 0, time.UTC),
	} {
		giftRedeemDayIsWhole(t, cutoff.String(), cutoff)
	}
	if printed := GiftRedeemByDate(time.Date(2027, 11, 14, 0, 0, 0, 0, time.UTC)); !printed.Equal(date(2027, 11, 12)) {
		t.Fatalf("a midnight UTC cutoff prints %s", printed.Format(time.DateOnly))
	}

	// a delivery at 9:00 local keeps daylight saving
	if deliver := GiftOrderDeliverTime(date(2027, 3, 14), "America/Chicago"); !deliver.Equal(time.Date(2027, 3, 14, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("Chicago on the day daylight saving starts: %s", deliver)
	}
	if location := GiftOrderLocation("Not/AZone"); location != time.UTC || GiftOrderLocation("") != time.UTC || GiftOrderLocation("Local") != time.UTC {
		t.Fatal("an unknown zone reads as UTC")
	}
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

// A pending order of the delivery and rail, with a synthetic payment key.
func giftWindowTestOrder(t testing.TB, ctx context.Context, delivery string, rail string, zone string, sendOn *time.Time) (*GiftOrder, string) {
	t.Helper()
	order := &GiftOrder{
		Quantity:       2,
		UnitPriceCents: Gift().UnitPriceCents(2),
		TotalCents:     2 * Gift().UnitPriceCents(2),
		Rail:           rail,
		Delivery:       delivery,
		BuyerEmail:     "giver@example.invalid",
		SendTimezone:   zone,
		SendOn:         sendOn,
		Message:        "Happy holidays",
		Signature:      "Sam",
		Language:       "en",
	}
	if delivery == GiftDeliveryThem {
		order.Recipients = []string{"a@example.invalid", "b@example.invalid"}
	}
	if err := CreateGiftOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	base := "cs_test_gift_window_" + server.NewId().String()
	if rail == GiftRailCrypto {
		intentId := server.NewId()
		if err := SetGiftOrderCryptoIntentId(ctx, order.GiftOrderId, intentId); err != nil {
			t.Fatal(err)
		}
		base = CryptoPaymentPurchaseEventId(intentId)
	} else if err := SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, base); err != nil {
		t.Fatal(err)
	}
	return order, base
}

// The store keeps the token's SHA-256 only, the token opens the order and the
// stored hash does not, and no token opens an order more than 400 days old.
func TestGiftOrderTokenIsStoredHashed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		order, _ := giftWindowTestOrder(t, ctx, GiftDeliveryMe, GiftRailStripe, "America/Chicago", nil)
		var stored []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT order_token_hash FROM gift_order WHERE gift_order_id = $1`, order.GiftOrderId).Scan(&stored))
		})
		want := sha256.Sum256([]byte(order.OrderToken))
		if !bytes.Equal(stored, want[:]) || bytes.Contains(stored, []byte(order.OrderToken)) {
			t.Fatalf("the order stores %x, want the token's SHA-256", stored)
		}
		if GetGiftOrder(ctx, order.GiftOrderId).OrderToken != "" {
			t.Fatal("a read carries the token")
		}
		if GetGiftOrderForToken(ctx, order.GiftOrderId, order.OrderToken) == nil {
			t.Fatal("the token does not open its order")
		}
		for _, token := range []string{hex.EncodeToString(stored), string(stored), "", order.OrderToken + "A"} {
			if GetGiftOrderForToken(ctx, order.GiftOrderId, token) != nil {
				t.Fatalf("%q opens the order", token)
			}
		}
		age := func(days int) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE gift_order SET create_time = $2 WHERE gift_order_id = $1`,
					order.GiftOrderId,
					server.NowUtc().Add(-time.Duration(days)*24*time.Hour),
				))
			})
		}
		age(399)
		if GetGiftOrderForToken(ctx, order.GiftOrderId, order.OrderToken) == nil {
			t.Fatal("the token does not open its order at 399 days")
		}
		age(401)
		if GetGiftOrderForToken(ctx, order.GiftOrderId, order.OrderToken) != nil {
			t.Fatal("the token opens its order after 400 days")
		}
	})
}

// Fulfilment sets every code's window and the order's: from the delivery for
// a scheduled delivery "them", from the payment otherwise (and for a date that
// has already passed), always through a whole printed day; the check reads it.
func TestGiftFulfilmentRedeemWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := Testing_GiftConfig()
		defer Testing_SetGiftConfig(config)()

		now := server.NowUtc()
		sendOn := giftOrderDate(now.AddDate(0, 3, 0))
		pastSendOn := giftOrderDate(now.AddDate(0, 0, -2))
		cases := []struct {
			name        string
			delivery    string
			zone        string
			sendOn      *time.Time
			windowStart time.Time
		}{
			{"them on a date in Los Angeles", GiftDeliveryThem, "America/Los_Angeles", &sendOn, GiftOrderDeliverTime(sendOn, "America/Los_Angeles")},
			{"them on a date in Tokyo", GiftDeliveryThem, "Asia/Tokyo", &sendOn, GiftOrderDeliverTime(sendOn, "Asia/Tokyo")},
			{"them right away", GiftDeliveryThem, "America/Los_Angeles", nil, now},
			{"them on a date already past", GiftDeliveryThem, "America/Los_Angeles", &pastSendOn, now},
			{"me", GiftDeliveryMe, "America/Los_Angeles", nil, now},
		}
		for _, c := range cases {
			order, base := giftWindowTestOrder(t, ctx, c.delivery, GiftRailStripe, c.zone, c.sendOn)
			var codes []*GiftCode
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				codes, _, err = FulfillGiftOrderInTx(tx, ctx, order.GiftOrderId, base, UsdToNanoCents(1), now)
				server.Raise(err)
			}, server.TxReadCommitted)
			want := GiftRedeemBy(c.windowStart, config.RedeemableFor, GiftOrderLocation(c.zone))
			for _, code := range append(codes, GetGiftOrderCodes(ctx, order.GiftOrderId)...) {
				if !code.RedeemBy.Equal(want) {
					t.Fatalf("%s: a code is redeemable until %s, want %s", c.name, code.RedeemBy, want)
				}
				status := CheckGiftCode(ctx, code.Secret)
				if status == nil || status.Status != GiftCodeStatusOk || status.RedeemBy == nil || !status.RedeemBy.Equal(want) {
					t.Fatalf("%s: the check reads %+v, want redeem by %s", c.name, status, want)
				}
			}
			if want.Sub(c.windowStart) < config.RedeemableFor {
				t.Fatalf("%s: the window is shorter than redeemable_for", c.name)
			}
		}
	})
}

// A card order still pending and an order of either rail refunded before it
// was ever paid lose their buyer's address; a pending crypto order (a late
// coin payment can still go to review) and an order once paid, refunded since
// or not, keep theirs.
func TestRemoveGiftOrderBuyerEmailOnlyForOrdersNeverPaid(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		refund := func(order *GiftOrder) {
			t.Helper()
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(SetGiftOrderRefundedInTx(tx, ctx, order.GiftOrderId))
			})
		}
		pay := func(order *GiftOrder, base string) {
			t.Helper()
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, err := FulfillGiftOrderInTx(tx, ctx, order.GiftOrderId, base, UsdToNanoCents(1), server.NowUtc())
				server.Raise(err)
			}, server.TxReadCommitted)
		}

		card, _ := giftWindowTestOrder(t, ctx, GiftDeliveryMe, GiftRailStripe, "America/Chicago", nil)
		coin, _ := giftWindowTestOrder(t, ctx, GiftDeliveryMe, GiftRailCrypto, "America/Chicago", nil)
		paid, paidBase := giftWindowTestOrder(t, ctx, GiftDeliveryMe, GiftRailStripe, "America/Chicago", nil)
		pay(paid, paidBase)
		paidRefunded, paidRefundedBase := giftWindowTestOrder(t, ctx, GiftDeliveryMe, GiftRailStripe, "America/Chicago", nil)
		pay(paidRefunded, paidRefundedBase)
		refund(paidRefunded)
		refunded, _ := giftWindowTestOrder(t, ctx, GiftDeliveryMe, GiftRailStripe, "America/Chicago", nil)
		refund(refunded)
		refundedCoin, _ := giftWindowTestOrder(t, ctx, GiftDeliveryMe, GiftRailCrypto, "America/Chicago", nil)
		refund(refundedCoin)
		for _, order := range []*GiftOrder{card, coin, paid, paidRefunded, refunded, refundedCoin} {
			RemoveGiftOrderBuyerEmail(ctx, order.GiftOrderId)
		}
		for _, check := range []struct {
			name  string
			order *GiftOrder
			want  string
		}{
			{"pending card", card, ""},
			{"pending coin", coin, "giver@example.invalid"},
			{"paid", paid, "giver@example.invalid"},
			{"paid then refunded", paidRefunded, "giver@example.invalid"},
			{"card refunded unpaid", refunded, ""},
			{"coin refunded unpaid", refundedCoin, ""},
		} {
			if got := GetGiftOrder(ctx, check.order.GiftOrderId).BuyerEmail; got != check.want {
				t.Fatalf("%s order %s (%s): buyer %q, want %q", check.name, check.order.GiftOrderId, fmt.Sprint(check.order.Rail), got, check.want)
			}
		}
		if after := GetGiftOrder(ctx, refunded.GiftOrderId); after.RefundedTime == nil || after.Status != GiftOrderStatusRefunded {
			t.Fatalf("the refund time is not read: %+v", after)
		}
	})
}
