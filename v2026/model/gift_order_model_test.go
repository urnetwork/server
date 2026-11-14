package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A pending order of quantity gifts, keyed to a synthetic Stripe session, as
// the checkout makes one.
func giftTestOrder(t testing.TB, ctx context.Context, quantity int, delivery string, referrerNetworkId *server.Id) *GiftOrder {
	t.Helper()
	order := &GiftOrder{
		Quantity:          quantity,
		UnitPriceCents:    Gift().UnitPriceCents(quantity),
		TotalCents:        int64(quantity) * Gift().UnitPriceCents(quantity),
		Rail:              GiftRailStripe,
		Delivery:          delivery,
		BuyerEmail:        "giver@example.invalid",
		Message:           "Happy holidays",
		Signature:         "Sam",
		Language:          "en",
		BittensorFooter:   false,
		ReferrerNetworkId: referrerNetworkId,
	}
	if delivery == GiftDeliveryThem {
		for i := 0; i < quantity; i += 1 {
			order.Recipients = append(order.Recipients, fmt.Sprintf("friend%d@example.invalid", i))
		}
	}
	if err := CreateGiftOrder(ctx, order); err != nil {
		t.Fatalf("create gift order: %v", err)
	}
	sessionId := "cs_test_gift_" + server.NewId().String()
	if err := SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, sessionId); err != nil {
		t.Fatalf("set gift order session: %v", err)
	}
	order.StripeSessionId = sessionId
	return order
}

// Fulfils the order in its own ReadCommitted transaction, as the webhook does.
func giftTestFulfill(t testing.TB, ctx context.Context, order *GiftOrder, purchaseEventBase string, netRevenue int64, now time.Time) (codes []*GiftCode, newlyPaid bool, err error) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		codes, newlyPaid, err = FulfillGiftOrderInTx(tx, ctx, order.GiftOrderId, purchaseEventBase, netRevenue, now)
	}, server.TxReadCommitted)
	return
}

// A paid order's codes, fulfilled now.
func giftTestCodes(t testing.TB, ctx context.Context, quantity int, referrerNetworkId *server.Id) (*GiftOrder, []*GiftCode) {
	t.Helper()
	order := giftTestOrder(t, ctx, quantity, GiftDeliveryMe, referrerNetworkId)
	codes, newlyPaid, err := giftTestFulfill(t, ctx, order, order.StripeSessionId, UsdToNanoCents(float64(order.TotalCents)/100), server.NowUtc())
	if err != nil || !newlyPaid || len(codes) != quantity {
		t.Fatalf("fulfil: %d codes, newly paid %t, %v", len(codes), newlyPaid, err)
	}
	return order, codes
}

// An order is stored with its recipients in order and read back whole; its
// token reads it only when it matches; its session and invoice find it; and a
// malformed order is refused before anything is stored.
func TestGiftOrderCreateAndRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		referrerNetworkId := server.NewId()
		sendOn := time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)
		order := &GiftOrder{
			Quantity:          3,
			UnitPriceCents:    2999,
			TotalCents:        8997,
			Rail:              GiftRailStripe,
			Delivery:          GiftDeliveryThem,
			BuyerEmail:        "giver@example.invalid",
			Recipients:        []string{"a@example.invalid", "b@example.invalid", "c@example.invalid"},
			SendOn:            &sendOn,
			SendTimezone:      "America/Chicago",
			Message:           "Stay private ☃",
			Signature:         "Sam",
			Language:          "es",
			BittensorFooter:   true,
			ReferrerNetworkId: &referrerNetworkId,
		}
		if err := CreateGiftOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		if order.Status != GiftOrderStatusPending || len(order.OrderToken) != 26 || order.CreateTime.IsZero() || order.Season != "winter-2026" {
			t.Fatalf("created %+v", order)
		}

		read := GetGiftOrder(ctx, order.GiftOrderId)
		if read == nil {
			t.Fatal("the order is not stored")
		}
		// the store keeps the token's SHA-256 only, so a read has no token
		if read.OrderToken != "" || read.Status != GiftOrderStatusPending || read.PaidTime != nil ||
			read.Quantity != 3 || read.UnitPriceCents != 2999 || read.TotalCents != 8997 || read.Rail != GiftRailStripe ||
			read.Delivery != GiftDeliveryThem || read.BuyerEmail != order.BuyerEmail || read.SendTimezone != "America/Chicago" ||
			read.Message != order.Message || read.Signature != "Sam" || read.Language != "es" || read.Season != "winter-2026" ||
			!read.BittensorFooter || read.ReferrerNetworkId == nil || *read.ReferrerNetworkId != referrerNetworkId || read.PdfReady ||
			read.StripeSessionId != "" || read.CryptoIntentId != nil {
			t.Fatalf("read %+v", read)
		}
		if read.SendOn == nil || !read.SendOn.Equal(sendOn) {
			t.Fatalf("send on %v, want %v", read.SendOn, sendOn)
		}
		if len(read.Recipients) != 3 || read.Recipients[0] != "a@example.invalid" || read.Recipients[2] != "c@example.invalid" {
			t.Fatalf("recipients %v", read.Recipients)
		}

		if GetGiftOrderForToken(ctx, order.GiftOrderId, order.OrderToken) == nil {
			t.Fatal("the order's token does not read it")
		}
		for _, token := range []string{"", "A", order.OrderToken + "A", order.OrderToken[1:] + "A"} {
			if GetGiftOrderForToken(ctx, order.GiftOrderId, token) != nil {
				t.Fatalf("token %q reads the order", token)
			}
		}
		if GetGiftOrderForToken(ctx, server.NewId(), order.OrderToken) != nil || GetGiftOrder(ctx, server.NewId()) != nil {
			t.Fatal("another id reads an order")
		}

		sessionId := "cs_test_" + server.NewId().String()
		if err := SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, sessionId); err != nil {
			t.Fatal(err)
		}
		if found := GetGiftOrderIdForStripeSession(ctx, sessionId); found == nil || *found != order.GiftOrderId {
			t.Fatalf("the session finds %v", found)
		}
		if GetGiftOrderIdForStripeSession(ctx, "cs_test_other") != nil || GetGiftOrderIdForStripeSession(ctx, "") != nil {
			t.Fatal("another session finds the order")
		}
		intentId := server.NewId()
		if err := SetGiftOrderCryptoIntentId(ctx, order.GiftOrderId, intentId); err != nil {
			t.Fatal(err)
		}
		if found := GetGiftOrderIdForCryptoIntent(ctx, intentId); found == nil || *found != order.GiftOrderId {
			t.Fatalf("the invoice finds %v", found)
		}
		if GetGiftOrderIdForCryptoIntent(ctx, server.NewId()) != nil {
			t.Fatal("another invoice finds the order")
		}
		if err := SetGiftOrderStripeSessionId(ctx, server.NewId(), "cs_test_none"); !errors.Is(err, ErrGiftOrderNotFound) {
			t.Fatalf("keying no order: %v", err)
		}
		if err := SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, string(make([]byte, 200))); err == nil {
			t.Fatal("a session too long to key the codes was accepted")
		}

		for name, bad := range map[string]*GiftOrder{
			"no gifts":           {Quantity: 0, Rail: GiftRailStripe, Delivery: GiftDeliveryMe},
			"recipients missing": {Quantity: 2, Rail: GiftRailStripe, Delivery: GiftDeliveryThem, Recipients: []string{"a@example.invalid"}},
			"unknown delivery":   {Quantity: 1, Rail: GiftRailStripe, Delivery: "post"},
			"unknown rail":       {Quantity: 1, Rail: "cash", Delivery: GiftDeliveryMe},
		} {
			if err := CreateGiftOrder(ctx, bad); err == nil {
				t.Errorf("%s: created", name)
			}
		}
	})
}

// Fulfilment creates the order's codes once: Quantity pro_gift codes keyed
// <session>/gift/<n>, the buyer's email, the revenue split to the nano cent,
// the giver as referrer, a year to redeem from the payment (through its last
// whole day everywhere), each
// assigned to its recipient; and the order paid. A redelivery returns the same
// codes and creates nothing; another purchase event is refused, as are a
// refunded order and an order with no gift config.
func TestFulfillGiftOrderIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		giverNetworkId := server.NewId()
		order := giftTestOrder(t, ctx, 3, GiftDeliveryThem, &giverNetworkId)
		now := server.NowUtc()
		// not a multiple of three: the split keeps every nano cent
		netRevenue := int64(8_997_000_000_002)

		codes, newlyPaid, err := giftTestFulfill(t, ctx, order, order.StripeSessionId, netRevenue, now)
		if err != nil || !newlyPaid || len(codes) != 3 {
			t.Fatalf("first fulfilment: %d codes, newly paid %t, %v", len(codes), newlyPaid, err)
		}
		// usable from the start of the purchase day; a year from the payment,
		// through the whole of its last day in every zone (GiftRedeemBy)
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		redeemBy := GiftRedeemBy(now, 8760*time.Hour, GiftOrderLocation(order.SendTimezone))
		for n, code := range codes {
			if len(code.Secret) != 26 || !code.RedeemBy.Equal(redeemBy) || code.RecipientIndex == nil || *code.RecipientIndex != n || code.Redeemed || code.Voided {
				t.Fatalf("code %d is %+v", n, code)
			}
		}

		revenue := int64(0)
		server.Db(ctx, func(conn server.PgConn) {
			for n, code := range codes {
				var kind, purchaseEventId, purchaseEmail string
				var proDurationSeconds int64
				var giftOrderId, referrerNetworkId *server.Id
				var codeRevenue, byteCount int64
				var startTime, endTime time.Time
				server.Raise(conn.QueryRow(
					ctx,
					`
						SELECT kind, purchase_event_id, purchase_email, pro_duration_seconds, gift_order_id,
							referrer_network_id, net_revenue_nano_cents, balance_byte_count, start_time, end_time
						FROM transfer_balance_code
						WHERE balance_code_id = $1
					`,
					code.BalanceCodeId,
				).Scan(&kind, &purchaseEventId, &purchaseEmail, &proDurationSeconds, &giftOrderId, &referrerNetworkId, &codeRevenue, &byteCount, &startTime, &endTime))
				if kind != BalanceCodeKindProGift || purchaseEventId != fmt.Sprintf("%s/gift/%d", order.StripeSessionId, n) ||
					purchaseEmail != order.BuyerEmail || proDurationSeconds != 8760*3600 || giftOrderId == nil || *giftOrderId != order.GiftOrderId ||
					referrerNetworkId == nil || *referrerNetworkId != giverNetworkId || byteCount != 0 ||
					!startTime.Equal(dayStart) || !endTime.Equal(redeemBy) {
					t.Fatalf("code %d row: %s %s %s %d %v %v %d %s %s", n, kind, purchaseEventId, purchaseEmail, proDurationSeconds, giftOrderId, referrerNetworkId, byteCount, startTime, endTime)
				}
				if codeRevenue < netRevenue/3 || netRevenue/3+1 < codeRevenue {
					t.Fatalf("code %d carries %d of %d", n, codeRevenue, netRevenue)
				}
				revenue += codeRevenue

				var recipientCodeId *server.Id
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT balance_code_id FROM gift_order_recipient WHERE gift_order_id = $1 AND recipient_index = $2`,
					order.GiftOrderId,
					n,
				).Scan(&recipientCodeId))
				if recipientCodeId == nil || *recipientCodeId != code.BalanceCodeId {
					t.Fatalf("recipient %d holds %v, want %s", n, recipientCodeId, code.BalanceCodeId)
				}
			}
		})
		if revenue != netRevenue {
			t.Fatalf("the codes carry %d of %d", revenue, netRevenue)
		}

		paid := GetGiftOrder(ctx, order.GiftOrderId)
		if paid.Status != GiftOrderStatusPaid || paid.PaidTime == nil || !paid.PaidTime.Equal(now.Truncate(time.Microsecond)) {
			t.Fatalf("paid order %+v", paid)
		}

		again, newlyPaid, err := giftTestFulfill(t, ctx, order, order.StripeSessionId, netRevenue, now.Add(time.Minute))
		if err != nil || newlyPaid || len(again) != 3 {
			t.Fatalf("redelivery: %d codes, newly paid %t, %v", len(again), newlyPaid, err)
		}
		for n := range codes {
			if again[n].BalanceCodeId != codes[n].BalanceCodeId || again[n].Secret != codes[n].Secret || !again[n].RedeemBy.Equal(redeemBy) ||
				again[n].RecipientIndex == nil || *again[n].RecipientIndex != n {
				t.Fatalf("redelivery code %d is %+v, want %+v", n, again[n], codes[n])
			}
		}
		listed := GetGiftOrderCodes(ctx, order.GiftOrderId)
		if len(listed) != 3 || listed[2].BalanceCodeId != codes[2].BalanceCodeId {
			t.Fatalf("listed %d codes", len(listed))
		}
		var codeCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM transfer_balance_code WHERE starts_with(purchase_event_id, $1)`,
				order.StripeSessionId+"/",
			).Scan(&codeCount))
		})
		if codeCount != 3 {
			t.Fatalf("the order has %d codes", codeCount)
		}
		if _, _, err := giftTestFulfill(t, ctx, order, "cs_test_another", netRevenue, now); err == nil {
			t.Fatal("a paid order was fulfilled for another purchase event")
		}

		// unpaid and unknown orders
		pending := giftTestOrder(t, ctx, 1, GiftDeliveryMe, nil)
		if listed := GetGiftOrderCodes(ctx, pending.GiftOrderId); listed == nil || len(listed) != 0 {
			t.Fatalf("an unpaid order lists %v", listed)
		}
		if GetGiftOrderCodes(ctx, server.NewId()) != nil {
			t.Fatal("no order lists codes")
		}
		if _, _, err := giftTestFulfill(t, ctx, &GiftOrder{GiftOrderId: server.NewId()}, "cs_test_none", 0, now); !errors.Is(err, ErrGiftOrderNotFound) {
			t.Fatalf("fulfilling no order: %v", err)
		}

		// a refunded order is never fulfilled
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(SetGiftOrderRefundedInTx(tx, ctx, pending.GiftOrderId))
			// again: already refunded
			server.Raise(SetGiftOrderRefundedInTx(tx, ctx, pending.GiftOrderId))
			if err := SetGiftOrderRefundedInTx(tx, ctx, server.NewId()); !errors.Is(err, ErrGiftOrderNotFound) {
				server.Raise(fmt.Errorf("refunding no order: %v", err))
			}
		})
		if GetGiftOrder(ctx, pending.GiftOrderId).Status != GiftOrderStatusRefunded {
			t.Fatal("the order is not refunded")
		}
		if _, _, err := giftTestFulfill(t, ctx, pending, pending.StripeSessionId, 0, now); !errors.Is(err, ErrGiftOrderRefunded) {
			t.Fatalf("fulfilling a refunded order: %v", err)
		}

		// no gift config: refused, nothing written
		unconfigured := giftTestOrder(t, ctx, 1, GiftDeliveryMe, nil)
		restore := Testing_SetGiftConfig(&GiftConfig{})
		_, _, err = giftTestFulfill(t, ctx, unconfigured, unconfigured.StripeSessionId, 0, now)
		restore()
		if err == nil || GetGiftOrder(ctx, unconfigured.GiftOrderId).Status != GiftOrderStatusPending {
			t.Fatalf("an order was fulfilled without a gift config: %v", err)
		}
	})
}

// The codes are keyed where the order's refund looks: an order keyed to a
// Stripe session is fulfilled only by that session, and one keyed to a crypto
// invoice only by crypto/<intent>, whose clawback then voids its codes.
func TestFulfillGiftOrderKeysTheRefundScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		stripeOrder := giftTestOrder(t, ctx, 1, GiftDeliveryMe, nil)
		if _, _, err := giftTestFulfill(t, ctx, stripeOrder, "cs_test_another_session", 0, server.NowUtc()); err == nil {
			t.Fatal("a Stripe order was fulfilled by another session")
		}
		if GetGiftOrder(ctx, stripeOrder.GiftOrderId).Status != GiftOrderStatusPending || len(GetGiftOrderCodes(ctx, stripeOrder.GiftOrderId)) != 0 {
			t.Fatal("a refused fulfilment wrote the order")
		}

		cryptoOrder := &GiftOrder{
			Quantity:       2,
			UnitPriceCents: 2999,
			TotalCents:     5998,
			Rail:           GiftRailCrypto,
			Delivery:       GiftDeliveryMe,
			BuyerEmail:     "giver@example.invalid",
			Language:       "en",
		}
		if err := CreateGiftOrder(ctx, cryptoOrder); err != nil {
			t.Fatal(err)
		}
		intentId := server.NewId()
		if err := SetGiftOrderCryptoIntentId(ctx, cryptoOrder.GiftOrderId, intentId); err != nil {
			t.Fatal(err)
		}
		if _, _, err := giftTestFulfill(t, ctx, cryptoOrder, intentId.String(), 0, server.NowUtc()); err == nil {
			t.Fatal("a crypto order was fulfilled by its bare intent id")
		}
		base := CryptoPaymentPurchaseEventId(intentId)
		codes, newlyPaid, err := giftTestFulfill(t, ctx, cryptoOrder, base, UsdToNanoCents(59.98), server.NowUtc())
		if err != nil || !newlyPaid || len(codes) != 2 {
			t.Fatalf("crypto fulfilment: %d codes, newly paid %t, %v", len(codes), newlyPaid, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			found, voided, _ := ClawbackBalanceCodesForPurchaseEventInTx(tx, ctx, base, server.NowUtc())
			if !found || voided != 2 {
				server.Raise(fmt.Errorf("the invoice's clawback found %t voided %d", found, voided))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		for _, code := range GetGiftOrderCodes(ctx, cryptoOrder.GiftOrderId) {
			if !code.Voided {
				t.Fatalf("code %s outlived its invoice's refund", code.BalanceCodeId)
			}
		}
	})
}

// An order's PDFs: "all" makes it ready, a recipient's card is stored by its
// index, other parts are refused, and the retention removes the PDFs and
// blanks the message, signature and recipients, keeping the codes.
func TestGiftOrderPdfAndRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		order := giftTestOrder(t, ctx, 2, GiftDeliveryThem, nil)
		if _, _, err := giftTestFulfill(t, ctx, order, order.StripeSessionId, 0, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		if GetGiftOrderPdf(ctx, order.GiftOrderId, GiftOrderPdfPartAll) != nil {
			t.Fatal("a PDF before any is stored")
		}
		if err := PutGiftOrderPdf(ctx, order.GiftOrderId, GiftOrderPdfRecipientPart(1), []byte("%PDF-r1")); err != nil {
			t.Fatal(err)
		}
		if GetGiftOrder(ctx, order.GiftOrderId).PdfReady {
			t.Fatal("a recipient's card made the order ready")
		}
		if err := PutGiftOrderPdf(ctx, order.GiftOrderId, GiftOrderPdfPartAll, []byte("%PDF-first")); err != nil {
			t.Fatal(err)
		}
		if err := PutGiftOrderPdf(ctx, order.GiftOrderId, GiftOrderPdfPartAll, []byte("%PDF-all")); err != nil {
			t.Fatal(err)
		}
		if !GetGiftOrder(ctx, order.GiftOrderId).PdfReady {
			t.Fatal("the combined PDF did not make the order ready")
		}
		if pdf := GetGiftOrderPdf(ctx, order.GiftOrderId, GiftOrderPdfPartAll); string(pdf) != "%PDF-all" {
			t.Fatalf("all is %q", pdf)
		}
		if pdf := GetGiftOrderPdf(ctx, order.GiftOrderId, "r1"); string(pdf) != "%PDF-r1" {
			t.Fatalf("r1 is %q", pdf)
		}
		for _, part := range []string{"r2", "r01", "x", "", "rall", "r-1"} {
			if err := PutGiftOrderPdf(ctx, order.GiftOrderId, part, []byte("%PDF")); err == nil {
				t.Errorf("part %q was stored", part)
			}
		}
		if err := PutGiftOrderPdf(ctx, order.GiftOrderId, GiftOrderPdfPartAll, nil); err == nil {
			t.Error("an empty PDF was stored")
		}
		if err := PutGiftOrderPdf(ctx, server.NewId(), GiftOrderPdfPartAll, []byte("%PDF")); !errors.Is(err, ErrGiftOrderNotFound) {
			t.Errorf("a PDF of no order: %v", err)
		}

		RemoveGiftOrderPersonalData(ctx, order.GiftOrderId)
		kept := GetGiftOrder(ctx, order.GiftOrderId)
		if kept.PdfReady || kept.Message != "" || kept.Signature != "" || len(kept.Recipients) != 2 || kept.Recipients[0] != "" || kept.BuyerEmail == "" {
			t.Fatalf("after retention %+v", kept)
		}
		if GetGiftOrderPdf(ctx, order.GiftOrderId, "r1") != nil {
			t.Fatal("a card outlived the retention")
		}
		if codes := GetGiftOrderCodes(ctx, order.GiftOrderId); len(codes) != 2 {
			t.Fatalf("the retention left %d codes", len(codes))
		}
	})
}

// The status of a code, by CheckGiftCode and on the check route: a gift code
// is ok until its redeem-by, then expired; redeemed once redeemed; voided once
// refunded unredeemed; an unknown secret is not found. A data code is ok or
// redeemed and never expires, and the check route keeps answering unknown for
// one that cannot be redeemed.
func TestCheckGiftCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-gift-check", userId)
		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "a", false, false))

		check := func(secret string) (*GiftCodeStatus, *CheckBalanceCodeResult) {
			result, err := CheckBalanceCode(&CheckBalanceCodeArgs{Secret: secret}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return CheckGiftCode(ctx, secret), result
		}

		_, codes := giftTestCodes(t, ctx, 3, nil)
		status, result := check(codes[0].Secret)
		if status.Kind != BalanceCodeKindProGift || status.Status != GiftCodeStatusOk || status.ProDays != 365 || status.RedeemBy == nil || !status.RedeemBy.Equal(codes[0].RedeemBy) {
			t.Fatalf("ok code status %+v", status)
		}
		if result.Error != nil || result.Balance == nil || result.Balance.BalanceByteCount != 0 || result.Kind != BalanceCodeKindProGift ||
			result.ProDays != 365 || result.RedeemBy == nil || !result.RedeemBy.Equal(codes[0].RedeemBy) || !result.Balance.EndTime.Equal(codes[0].RedeemBy) {
			t.Fatalf("ok code check %+v", result)
		}

		redeemed, err := RedeemBalanceCode(&RedeemBalanceCodeArgs{Secret: codes[0].Secret, NetworkId: networkId}, ctx)
		if err != nil || redeemed.Error != nil {
			t.Fatalf("redeem: %+v %v", redeemed, err)
		}
		status, result = check(codes[0].Secret)
		if status.Status != GiftCodeStatusRedeemed || status.RedeemBy == nil || !status.RedeemBy.Equal(codes[0].RedeemBy) {
			t.Fatalf("redeemed code status %+v", status)
		}
		if result.Balance != nil || result.Error == nil || result.Error.Message != balanceCodeRedeemedMessage || result.Kind != BalanceCodeKindProGift {
			t.Fatalf("redeemed code check %+v", result)
		}

		voidedOrder, voidedCodes := giftTestCodes(t, ctx, 1, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			found, voided, _ := ClawbackBalanceCodesForPurchaseEventInTx(tx, ctx, voidedOrder.StripeSessionId, server.NowUtc())
			if !found || voided != 1 {
				server.Raise(fmt.Errorf("clawback found %t voided %d", found, voided))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		status, result = check(voidedCodes[0].Secret)
		if status.Status != GiftCodeStatusVoided || result.Error == nil || result.Error.Message != balanceCodeVoidedMessage {
			t.Fatalf("voided code: %+v %+v", status, result)
		}

		expiredOrder := giftTestOrder(t, ctx, 1, GiftDeliveryMe, nil)
		expiredCodes, _, err := giftTestFulfill(t, ctx, expiredOrder, expiredOrder.StripeSessionId, 0, server.NowUtc().Add(-400*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		status, result = check(expiredCodes[0].Secret)
		if status.Status != GiftCodeStatusExpired || status.RedeemBy == nil || !server.NowUtc().After(*status.RedeemBy) {
			t.Fatalf("expired code status %+v", status)
		}
		if result.Error == nil || result.Error.Message != balanceCodeExpiredMessage || result.Kind != BalanceCodeKindProGift || result.RedeemBy == nil {
			t.Fatalf("expired code check %+v", result)
		}

		status, result = check("NOTACODE234567ABCDEFGHIJKL")
		if status.Status != GiftCodeStatusNotFound || status.Kind != "" || result.Error == nil || result.Error.Message != balanceCodeUnknownMessage || result.Kind != "" {
			t.Fatalf("unknown code: %+v %+v", status, result)
		}

		dataCode, err := CreateBalanceCode(ctx, 1024, 365*24*time.Hour, 100, "synthetic-data-check-"+server.NewId().String(), "receipt", "buyer@example.invalid")
		if err != nil {
			t.Fatal(err)
		}
		status, result = check(dataCode.Secret)
		if status.Kind != BalanceCodeKindData || status.Status != GiftCodeStatusOk || status.ProDays != 0 || status.RedeemBy != nil {
			t.Fatalf("data code status %+v", status)
		}
		if result.Error != nil || result.Balance == nil || result.Balance.BalanceByteCount != 1024 || result.Kind != BalanceCodeKindData || result.ProDays != 0 || result.RedeemBy != nil {
			t.Fatalf("data code check %+v", result)
		}
		if redeemed, err := RedeemBalanceCode(&RedeemBalanceCodeArgs{Secret: dataCode.Secret, NetworkId: networkId}, ctx); err != nil || redeemed.Error != nil || redeemed.Pro != nil {
			t.Fatalf("data redeem: %+v %v", redeemed, err)
		}
		status, result = check(dataCode.Secret)
		if status.Kind != BalanceCodeKindData || status.Status != GiftCodeStatusRedeemed {
			t.Fatalf("redeemed data code status %+v", status)
		}
		if result.Error == nil || result.Error.Message != balanceCodeUnknownMessage || result.Kind != "" || result.Balance != nil {
			t.Fatalf("redeemed data code check %+v", result)
		}
	})
}
