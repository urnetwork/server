package model

// Database tests of a gift order paid with Gamma (gift_order_model.go,
// gift_refund_model.go, gift_unlink_model.go): the order's link to its intent,
// its fulfilment under the intent's purchase event base, final sale against
// every refund and dispute path, and the 30-day unlink. Needs the test
// database (server.DefaultTestEnv).

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A network for the intents to belong to: an intent is recorded only for a
// network that exists.
func giftGammaTestNetwork(t testing.TB, ctx context.Context) server.Id {
	t.Helper()
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "giftgamma"+strings.ReplaceAll(networkId.String(), "-", "")[20:], server.NewId())
	return networkId
}

// A gift order's Gamma intent of the network, made at createTime and open for
// a day, priced at the order's total, and the order of quantity gifts linked
// to it in the intent's transaction, as POST /pay/gift/gamma-intent records
// them.
func giftGammaTestOrder(t testing.TB, ctx context.Context, networkId server.Id, quantity int, delivery string, createTime time.Time) (*GiftOrder, *GammaPaymentIntent) {
	t.Helper()
	unitPriceCents := Gift().UnitPriceCents(quantity)
	order := &GiftOrder{
		Quantity:       quantity,
		UnitPriceCents: unitPriceCents,
		TotalCents:     unitPriceCents * int64(quantity),
		Rail:           GiftRailGamma,
		Delivery:       delivery,
		BuyerEmail:     "giver@example.invalid",
		Message:        "Happy holidays",
		Signature:      "Sam",
		Language:       "en",
	}
	if delivery == GiftDeliveryThem {
		for i := range quantity {
			order.Recipients = append(order.Recipients, fmt.Sprintf("friend%d@example.invalid", i))
		}
	}
	if err := CreateGiftOrder(ctx, order); err != nil {
		t.Fatalf("create gift order: %v", err)
	}
	reference := make([]byte, 32)
	if _, err := rand.Read(reference); err != nil {
		t.Fatal(err)
	}
	price := UsdToNanoCents(float64(order.TotalCents) / 100)
	intent := &GammaPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          reference,
		NetworkId:                networkId,
		Hotkey:                   "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY",
		Purpose:                  GammaPurposePurchase,
		Reference:                reference,
		Delivery:                 "code",
		Item:                     "gift_pro_year",
		PriceUsdNanoCents:        price,
		RegularPriceUsdNanoCents: price,
		CreateTime:               createTime,
		ExpiresAt:                createTime.Add(24 * time.Hour),
	}
	var linkErr error
	created := false
	server.Tx(ctx, func(tx server.PgTx) {
		created = CreateGammaPaymentIntentInTx(tx, ctx, intent)
		linkErr = SetGiftOrderGammaIntentIdInTx(tx, ctx, order.GiftOrderId, intent.IntentId)
	})
	if !created || linkErr != nil {
		t.Fatalf("record the intent and its link: created %t, %v", created, linkErr)
	}
	return GetGiftOrder(ctx, order.GiftOrderId), intent
}

// Credits the intent at creditTime and fulfils its order, as the Gamma credit
// does, in one transaction.
func giftGammaTestCredit(t testing.TB, ctx context.Context, order *GiftOrder, intent *GammaPaymentIntent, creditTime time.Time) []*GiftCode {
	t.Helper()
	var codes []*GiftCode
	server.Tx(ctx, func(tx server.PgTx) {
		if !MarkGammaPaymentIntentCreditedInTx(tx, ctx, intent.IntentId, creditTime) {
			t.Fatal("the intent's one-shot was taken")
		}
		var newlyPaid bool
		var err error
		codes, newlyPaid, err = FulfillGiftOrderInTx(tx, ctx, order.GiftOrderId, GammaPaymentPurchaseEventId(intent.IntentId), intent.PriceUsdNanoCents, creditTime)
		if err != nil || !newlyPaid || len(codes) != order.Quantity {
			t.Fatalf("fulfil: %d codes, newly paid %t, %v", len(codes), newlyPaid, err)
		}
	}, server.TxReadCommitted)
	return codes
}

// The start of t's day, UTC.
func giftGammaTestDay(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func giftGammaTestExec(ctx context.Context, sql string, args ...any) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, sql, args...))
	})
}

func giftGammaTestCount(ctx context.Context, sql string, args ...any) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, sql, args...).Scan(&count))
	})
	return
}

// A Gamma order is linked to its intent once, in the intent's transaction,
// only while it is a pending gamma order; the intent finds it; it is a
// started order; and it is fulfilled only under the intent's purchase event
// base, gamma/<intent id>.
func TestGiftOrderGammaLink(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()
		networkId := giftGammaTestNetwork(t, ctx)

		order, intent := giftGammaTestOrder(t, ctx, networkId, 3, GiftDeliveryMe, server.NowUtc())
		if order.Rail != GiftRailGamma || order.GammaIntentId == nil || *order.GammaIntentId != intent.IntentId ||
			order.CryptoIntentId != nil || order.StripeSessionId != "" || order.Status != GiftOrderStatusPending {
			t.Fatalf("order %+v", order)
		}
		if found := GetGiftOrderIdForGammaIntent(ctx, intent.IntentId); found == nil || *found != order.GiftOrderId {
			t.Fatalf("the intent finds %v", found)
		}
		if GetGiftOrderIdForGammaIntent(ctx, server.NewId()) != nil {
			t.Fatal("an intent of no order finds one")
		}
		// a started order is never removed
		if RemoveUnstartedGiftOrder(ctx, order.GiftOrderId) {
			t.Fatal("an order linked to its intent was removed as unstarted")
		}

		// a refusal is an error, or the raise of the unique link, which rolls
		// the intent's transaction back either way
		link := func(giftOrderId server.Id, intentId server.Id) (err error) {
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					err = SetGiftOrderGammaIntentIdInTx(tx, ctx, giftOrderId, intentId)
				}, server.OptNoRetry())
			}, func(raised error) {
				err = raised
			})
			return
		}
		// once: neither the same intent again nor another
		if link(order.GiftOrderId, intent.IntentId) == nil || link(order.GiftOrderId, server.NewId()) == nil {
			t.Fatal("an order was linked twice")
		}
		// one intent pays one order
		other := &GiftOrder{Quantity: 1, UnitPriceCents: 3999, TotalCents: 3999, Rail: GiftRailGamma, Delivery: GiftDeliveryMe, BuyerEmail: "other@example.invalid", Language: "en"}
		if err := CreateGiftOrder(ctx, other); err != nil {
			t.Fatal(err)
		}
		if err := link(other.GiftOrderId, intent.IntentId); err == nil {
			t.Fatal("one intent was linked to two orders")
		}
		// only a gamma order
		card := giftTestOrder(t, ctx, 1, GiftDeliveryMe, nil)
		if err := link(card.GiftOrderId, server.NewId()); err == nil {
			t.Fatal("a card order was linked to a gamma intent")
		}
		if err := link(server.NewId(), server.NewId()); !errors.Is(err, ErrGiftOrderNotFound) {
			t.Fatalf("no order: %v", err)
		}

		// fulfilled only under the intent's base
		if _, _, err := giftTestFulfill(t, ctx, order, CryptoPaymentPurchaseEventId(intent.IntentId), 1, server.NowUtc()); err == nil {
			t.Fatal("a gamma order was fulfilled under a crypto invoice's base")
		}
		codes := giftGammaTestCredit(t, ctx, order, intent, server.NowUtc())
		for n, code := range codes {
			balanceCode, err := GetBalanceCode(ctx, code.BalanceCodeId)
			if err != nil || balanceCode.PurchaseEventId != fmt.Sprintf("gamma/%s/gift/%d", intent.IntentId, n) {
				t.Fatalf("code %d: %+v %v", n, balanceCode, err)
			}
		}
		paid := GetGiftOrder(ctx, order.GiftOrderId)
		if paid.Status != GiftOrderStatusPaid || paid.PaidTime == nil {
			t.Fatalf("paid %+v", paid)
		}
		// a paid order is linked to nothing else
		if err := link(order.GiftOrderId, server.NewId()); err == nil {
			t.Fatal("a paid order was linked again")
		}
	})
}

// A gift order paid with Gamma is final sale: no refund, dispute or operator
// refund voids a code, ends a year or records anything, and each says so,
// while a card order is not.
func TestGiftGammaOrderIsFinalSale(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()
		networkId := giftGammaTestNetwork(t, ctx)
		order, intent := giftGammaTestOrder(t, ctx, networkId, 2, GiftDeliveryMe, server.NowUtc())
		codes := giftGammaTestCredit(t, ctx, order, intent, server.NowUtc())
		now := server.NowUtc()

		server.Tx(ctx, func(tx server.PgTx) {
			if !GiftOrderFinalSaleInTx(tx, ctx, order.GiftOrderId) {
				t.Fatal("a gamma order is not final sale")
			}
			outcome := RefundGiftOrderCodesInTx(tx, ctx, order.GiftOrderId, "re_gamma_"+server.NewId().String(), 3999, now)
			if outcome == nil || !outcome.FinalSale || 0 < len(outcome.VoidedCodeIds) || outcome.OrderRefunded || outcome.RefundedCents != 0 {
				t.Fatalf("refund %+v", outcome)
			}
			if voided := DisputeGiftOrderCodesInTx(tx, ctx, order.GiftOrderId, "dp_gamma", now); len(voided) != 0 {
				t.Fatalf("a dispute voided %d codes", len(voided))
			}
			if loss := LoseGiftOrderDisputeInTx(tx, ctx, order.GiftOrderId, "dp_gamma", now); loss != nil {
				t.Fatalf("a lost dispute ended %+v", loss)
			}
			if restoration := WinGiftOrderDisputeInTx(tx, ctx, order.GiftOrderId, "dp_gamma"); restoration != nil {
				t.Fatalf("a won dispute restored %+v", restoration)
			}
			refund := RefundCryptoGiftOrderInTx(tx, ctx, order.GiftOrderId, now)
			if refund == nil || !refund.FinalSale || refund.Refunded != 0 {
				t.Fatalf("operator refund %+v", refund)
			}
			if networkIds, ended := endCryptoGiftOrderGrantInTx(tx, ctx, order.GiftOrderId, now); ended || 0 < len(networkIds) {
				t.Fatal("an orphan leg ended a gamma order's codes")
			}
		}, server.TxReadCommitted, server.OptNoRetry())

		if read := GetGiftOrder(ctx, order.GiftOrderId); read.Status != GiftOrderStatusPaid || read.RefundedTime != nil {
			t.Fatalf("the order is %s", read.Status)
		}
		for _, code := range codes {
			if status := CheckGiftCode(ctx, code.Secret); status.Status != GiftCodeStatusOk {
				t.Fatalf("code %s is %s", code.BalanceCodeId, status.Status)
			}
		}
		if count := giftGammaTestCount(ctx, `SELECT count(*) FROM gift_code_void WHERE gift_order_id = $1`, order.GiftOrderId); count != 0 {
			t.Fatalf("%d voids recorded", count)
		}
		if count := giftGammaTestCount(ctx, `SELECT count(*) FROM gift_order_refund WHERE gift_order_id = $1`, order.GiftOrderId); count != 0 {
			t.Fatalf("%d refunds recorded", count)
		}

		// a card order is not final sale
		card, _ := giftTestCodes(t, ctx, 1, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			if GiftOrderFinalSaleInTx(tx, ctx, card.GiftOrderId) || GiftOrderFinalSaleInTx(tx, ctx, server.NewId()) {
				t.Fatal("a card order, or no order, is final sale")
			}
		})
	})
}

// 30 days on, an order forgets its Gamma intent as a coin's order forgets its
// invoice: its codes move to a fresh base and still read and redeem, its
// times become their day, its referrer and its buyer's email (once no friend's
// email is owed) go. An intent that can still be charged or credited keeps
// its order, and so does one credited or made since the cutoff. A voided
// intent is unlinked 30 days after its void, whatever its charge (pending in
// no batch, as the account's deletion leaves it, or spent and disposed of by
// an operator), unless that charge is in a live batch, which may still land.
func TestUnlinkGammaGiftOrders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()
		networkId := giftGammaTestNetwork(t, ctx)
		now := server.NowUtc()
		cutoff := now.Add(-GiftGammaUnlinkAfter)
		old := now.Add(-GiftGammaUnlinkAfter - 48*time.Hour)

		// paid 32 days ago, every card to the buyer: unlinked whole
		paid, paidIntent := giftGammaTestOrder(t, ctx, networkId, 3, GiftDeliveryMe, old)
		referrerNetworkId := server.NewId()
		giftGammaTestExec(ctx, `UPDATE gift_order SET referrer_network_id = $2 WHERE gift_order_id = $1`, paid.GiftOrderId, referrerNetworkId)
		paidCodes := giftGammaTestCredit(t, ctx, paid, paidIntent, old.Add(time.Hour))
		// paid 32 days ago, a friend's card not yet sent: unlinked, the
		// buyer's address kept as its reply-to
		owed, owedIntent := giftGammaTestOrder(t, ctx, networkId, 2, GiftDeliveryThem, old)
		giftGammaTestCredit(t, ctx, owed, owedIntent, old.Add(time.Hour))
		// paid since the cutoff: kept
		recent, recentIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		giftGammaTestCredit(t, ctx, recent, recentIntent, now.Add(-time.Hour))
		// never paid, its intent expired 31 days ago: unlinked
		expired, _ := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		// never paid, voided: unlinked
		voided, voidedIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		giftGammaTestExec(ctx, `UPDATE gamma_payment_intent SET voided_at = $2 WHERE intent_id = $1`, voidedIntent.IntentId, old.Add(time.Hour))
		// spent and not credited: its credit needs the link, kept
		spent, spentIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		if !CreateGammaCharge(ctx, spentIntent, old.Add(time.Minute)) {
			t.Fatal("no charge")
		}
		giftGammaTestExec(ctx, `UPDATE gamma_charge SET state = 'spent' WHERE intent_id = $1`, spentIntent.IntentId)
		// a charge still pending: kept
		pending, pendingIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		if !CreateGammaCharge(ctx, pendingIntent, old.Add(time.Minute)) {
			t.Fatal("no charge")
		}
		// made before the cutoff but still open: kept
		open, openIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		giftGammaTestExec(ctx, `UPDATE gamma_payment_intent SET expires_at = $2 WHERE intent_id = $1`, openIntent.IntentId, now.Add(time.Hour))
		// voided by the account's deletion, which leaves its pending charge in
		// no batch: that charge never joins one, so unlinked
		deleted, deletedIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		if !CreateGammaCharge(ctx, deletedIntent, old.Add(time.Minute)) {
			t.Fatal("no charge")
		}
		giftGammaTestExec(ctx, `UPDATE gamma_payment_intent SET voided_at = $2 WHERE intent_id = $1`, deletedIntent.IntentId, old.Add(time.Hour))
		// spent, its credit kept failing, voided by an operator's disposition:
		// unlinked
		disposed, disposedIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		if !CreateGammaCharge(ctx, disposedIntent, old.Add(time.Minute)) {
			t.Fatal("no charge")
		}
		giftGammaTestExec(ctx, `UPDATE gamma_charge SET state = 'spent', block_number = 9, resolved_at = $2 WHERE intent_id = $1`, disposedIntent.IntentId, old.Add(time.Minute))
		giftGammaTestExec(ctx, `UPDATE gamma_payment_intent SET voided_at = $2, credit_failure_count = 10 WHERE intent_id = $1`, disposedIntent.IntentId, old.Add(2*time.Hour))
		// voided while its charge is in a live spend batch, which may still
		// land: kept
		landing, landingIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		if !CreateGammaCharge(ctx, landingIntent, old.Add(time.Minute)) {
			t.Fatal("no charge")
		}
		giftGammaTestBatch(t, ctx, landingIntent)
		giftGammaTestExec(ctx, `UPDATE gamma_payment_intent SET voided_at = $2 WHERE intent_id = $1`, landingIntent.IntentId, old.Add(time.Hour))
		// voided since the cutoff: kept, for the 30 days after its void
		lately, latelyIntent := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, old)
		giftGammaTestExec(ctx, `UPDATE gamma_payment_intent SET voided_at = $2 WHERE intent_id = $1`, latelyIntent.IntentId, now.Add(-time.Hour))

		unlinked := UnlinkGammaGiftOrders(ctx, cutoff, 100, now)
		if unlinked != 6 {
			t.Fatalf("unlinked %d orders, want 6", unlinked)
		}
		if again := UnlinkGammaGiftOrders(ctx, cutoff, 100, now); again != 0 {
			t.Fatalf("unlinked %d more", again)
		}

		for _, kept := range []*GiftOrder{recent, spent, pending, open, landing, lately} {
			read := GetGiftOrder(ctx, kept.GiftOrderId)
			if read.GammaIntentId == nil || *read.GammaIntentId != *kept.GammaIntentId || read.BuyerEmail == "" {
				t.Fatalf("order %s was unlinked: %+v", kept.GiftOrderId, read)
			}
		}

		read := GetGiftOrder(ctx, paid.GiftOrderId)
		if read.GammaIntentId != nil || read.ReferrerNetworkId != nil || read.BuyerEmail != "" || read.Status != GiftOrderStatusPaid {
			t.Fatalf("the paid order after the unlink: %+v", read)
		}
		// its times are their day, as an unlinked invoice's are
		if !read.CreateTime.Equal(giftGammaTestDay(read.CreateTime)) ||
			read.PaidTime == nil || !read.PaidTime.Equal(giftGammaTestDay(old.Add(time.Hour))) {
			t.Fatalf("times %s %v", read.CreateTime, read.PaidTime)
		}
		if found := GetGiftOrderIdForGammaIntent(ctx, paidIntent.IntentId); found != nil {
			t.Fatal("the intent still finds its order")
		}
		codes := GetGiftOrderCodes(ctx, paid.GiftOrderId)
		if len(codes) != 3 {
			t.Fatalf("%d codes after the unlink", len(codes))
		}
		for n, code := range codes {
			if code.BalanceCodeId != paidCodes[n].BalanceCodeId || code.Secret != paidCodes[n].Secret {
				t.Fatalf("code %d changed", n)
			}
			balanceCode, err := GetBalanceCode(ctx, code.BalanceCodeId)
			if err != nil || !strings.HasPrefix(balanceCode.PurchaseEventId, "gamma/unlinked/") || strings.Contains(balanceCode.PurchaseEventId, paidIntent.IntentId.String()) ||
				balanceCode.PurchaseRecord != "" || balanceCode.PurchaseEmail != "" {
				t.Fatalf("code %d after the unlink: %+v %v", n, balanceCode, err)
			}
			if status := CheckGiftCode(ctx, code.Secret); status.Status != GiftCodeStatusOk {
				t.Fatalf("code %d is %s", n, status.Status)
			}
		}
		// the codes keep the referrer the redemption credits
		if count := giftGammaTestCount(ctx, `SELECT count(*) FROM transfer_balance_code WHERE gift_order_id = $1 AND referrer_network_id = $2`, paid.GiftOrderId, referrerNetworkId); count != 3 {
			t.Fatalf("%d codes keep their referrer", count)
		}

		// a friend's email still owed: the buyer's address stays its reply-to
		read = GetGiftOrder(ctx, owed.GiftOrderId)
		if read.GammaIntentId != nil || read.BuyerEmail != "giver@example.invalid" {
			t.Fatalf("the owed order after the unlink: %+v", read)
		}
		// printed, with no friend's email left unsent: forgotten
		if err := PutGiftOrderPdf(ctx, owed.GiftOrderId, GiftOrderPdfPartAll, []byte("%PDF-1.7 synthetic")); err != nil {
			t.Fatal(err)
		}
		if !ClearUnlinkedGiftOrderBuyerEmail(ctx, owed.GiftOrderId) || GetGiftOrder(ctx, owed.GiftOrderId).BuyerEmail != "" {
			t.Fatal("the buyer's address of an unlinked order outlived its last friend's email")
		}

		for _, never := range []*GiftOrder{expired, voided, deleted, disposed} {
			read := GetGiftOrder(ctx, never.GiftOrderId)
			if read.GammaIntentId != nil || read.BuyerEmail != "" || read.Status != GiftOrderStatusPending || read.PaidTime != nil {
				t.Fatalf("a never paid order after the unlink: %+v", read)
			}
		}
		// a linked order's buyer is never forgotten as unlinked
		if ClearUnlinkedGiftOrderBuyerEmail(ctx, recent.GiftOrderId) {
			t.Fatal("a linked order's buyer was forgotten")
		}
	})
}

// An intent of the network that is not a gift's: a data pack, open for a day.
func giftGammaTestDataIntent(t testing.TB, ctx context.Context, networkId server.Id, createTime time.Time) *GammaPaymentIntent {
	t.Helper()
	reference := make([]byte, 32)
	if _, err := rand.Read(reference); err != nil {
		t.Fatal(err)
	}
	intent := &GammaPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          reference,
		NetworkId:                networkId,
		Hotkey:                   "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY",
		Purpose:                  GammaPurposePurchase,
		Reference:                reference,
		Delivery:                 "code",
		Item:                     "data_1tib",
		PriceUsdNanoCents:        UsdToNanoCents(5),
		RegularPriceUsdNanoCents: UsdToNanoCents(5),
		CreateTime:               createTime,
		ExpiresAt:                createTime.Add(24 * time.Hour),
	}
	if !CreateGammaPaymentIntent(ctx, intent) {
		t.Fatal("the data intent was not recorded")
	}
	return intent
}

// Puts the intents' pending charges in a live spend batch, as the biller does
// before it submits one: their spend is in flight.
func giftGammaTestBatch(t testing.TB, ctx context.Context, intents ...*GammaPaymentIntent) server.Id {
	t.Helper()
	references := [][]byte{}
	for _, intent := range intents {
		references = append(references, intent.Reference)
	}
	batch := &GammaSpendBatch{
		BatchId:       server.NewId(),
		Nonce:         1,
		EraBirth:      100,
		EraPeriod:     64,
		Extrinsic:     []byte{0x01},
		ExtrinsicHash: "0x" + strings.Repeat("ab", 32),
		CreateTime:    server.NowUtc(),
	}
	if err := CreateGammaSpendBatch(ctx, batch, references); err != nil {
		t.Fatalf("batch: %v", err)
	}
	return batch.BatchId
}

func giftGammaTestChargeState(ctx context.Context, intentId server.Id) (state string) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT state FROM gamma_charge WHERE intent_id = $1`, intentId).Scan(&state))
	})
	return
}

// A new gift purchase voids the network's other open gift purchases in its
// transaction, with every charge of theirs that could still be charged (a
// pending one in no batch, a skipped one), and never a plan or data purchase
// or another network's. While a gift purchase's spend is in flight (in a spend
// batch, or spent and not credited), nothing is voided and the new purchase is
// refused. A voided intent is never charged again.
func TestVoidOpenGammaGiftIntents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()
		networkId := giftGammaTestNetwork(t, ctx)
		otherNetworkId := giftGammaTestNetwork(t, ctx)
		now := server.NowUtc()
		void := func() (voided []server.Id, inFlight bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				voided, inFlight = VoidOpenGammaGiftIntentsInTx(tx, ctx, networkId, now)
			}, server.TxReadCommitted)
			return
		}

		_, plain := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, now)
		_, charged := giftGammaTestOrder(t, ctx, networkId, 2, GiftDeliveryMe, now)
		if !CreateGammaCharge(ctx, charged, now) {
			t.Fatal("no charge")
		}
		_, skipped := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, now)
		if !CreateGammaCharge(ctx, skipped, now) {
			t.Fatal("no charge")
		}
		giftGammaTestExec(ctx, `UPDATE gamma_charge SET state = 'skipped', resolved_at = $2, block_number = 7 WHERE intent_id = $1`, skipped.IntentId, now)
		data := giftGammaTestDataIntent(t, ctx, networkId, now)
		_, others := giftGammaTestOrder(t, ctx, otherNetworkId, 1, GiftDeliveryMe, now)

		voided, inFlight := void()
		if inFlight || len(voided) != 3 {
			t.Fatalf("voided %v, in flight %t", voided, inFlight)
		}
		for _, intent := range []*GammaPaymentIntent{plain, charged, skipped} {
			if read := GetGammaPaymentIntent(ctx, intent.IntentId); read.VoidedAt == nil || read.CreditedAt != nil {
				t.Fatalf("intent %s was not voided", intent.IntentId)
			}
		}
		if state := giftGammaTestChargeState(ctx, charged.IntentId); state != GammaChargeVoided {
			t.Fatalf("the pending charge is %s", state)
		}
		if state := giftGammaTestChargeState(ctx, skipped.IntentId); state != GammaChargeVoided {
			t.Fatalf("the skipped charge is %s", state)
		}
		// a plan or data purchase, and another network's, are never touched
		if GetGammaPaymentIntent(ctx, data.IntentId).VoidedAt != nil || GetGammaPaymentIntent(ctx, others.IntentId).VoidedAt != nil {
			t.Fatal("a data purchase or another network's gift was voided")
		}
		// a voided intent is never charged: no charge is made, none is
		// batched, none re-armed
		if CreateGammaCharge(ctx, plain, now) {
			t.Fatal("a voided intent was charged")
		}
		if ReArmGammaCharge(ctx, skipped.Reference) {
			t.Fatal("a voided intent's skipped charge was re-armed")
		}
		for _, charge := range ListUnbatchedGammaCharges(ctx, 100) {
			if charge.IntentId == charged.IntentId {
				t.Fatal("a voided intent's charge is listed for a batch")
			}
		}
		if err := CreateGammaSpendBatch(ctx, &GammaSpendBatch{
			BatchId: server.NewId(), Nonce: 1, EraBirth: 1, EraPeriod: 64, Extrinsic: []byte{1},
			ExtrinsicHash: "0x" + strings.Repeat("cd", 32), CreateTime: now,
		}, [][]byte{charged.Reference}); !errors.Is(err, ErrGammaChargeNotPayable) {
			t.Fatalf("a voided intent's charge joined a batch: %v", err)
		}
		for _, intent := range ListChargeableGammaPaymentIntents(ctx, now, 1000) {
			if intent.IntentId == plain.IntentId {
				t.Fatal("a voided intent is chargeable")
			}
		}

		// nothing open now: nothing to void
		if voided, inFlight := void(); 0 < len(voided) || inFlight {
			t.Fatalf("voided %v again", voided)
		}

		// a spend in a batch is in flight: nothing is voided
		_, batched := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, now)
		_, waiting := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, now)
		if !CreateGammaCharge(ctx, batched, now) {
			t.Fatal("no charge")
		}
		giftGammaTestBatch(t, ctx, batched)
		if !HasGammaGiftChargeInFlight(ctx, networkId) || HasGammaGiftChargeInFlight(ctx, otherNetworkId) {
			t.Fatal("the in-flight spend is not seen, or seen for another network")
		}
		if voided, inFlight := void(); !inFlight || 0 < len(voided) {
			t.Fatalf("voided %v beside a spend in flight", voided)
		}
		if GetGammaPaymentIntent(ctx, waiting.IntentId).VoidedAt != nil || GetGammaPaymentIntent(ctx, batched.IntentId).VoidedAt != nil {
			t.Fatal("an intent was voided beside a spend in flight")
		}
		// spent and not yet credited: still in flight
		giftGammaTestExec(ctx, `UPDATE gamma_charge SET state = 'spent', block_number = 8, resolved_at = $2 WHERE intent_id = $1`, batched.IntentId, now)
		if voided, inFlight := void(); !inFlight || 0 < len(voided) {
			t.Fatalf("voided %v beside a spent charge not yet credited", voided)
		}
		// credited: the other open one is voided
		server.Tx(ctx, func(tx server.PgTx) {
			MarkGammaPaymentIntentCreditedInTx(tx, ctx, batched.IntentId, now)
		})
		if HasGammaGiftChargeInFlight(ctx, networkId) {
			t.Fatal("a credited spend is in flight")
		}
		if voided, inFlight := void(); inFlight || len(voided) != 1 || voided[0] != waiting.IntentId {
			t.Fatalf("voided %v, in flight %t", voided, inFlight)
		}
	})
}

// Cancelling an order's gift purchase voids it, once, unless its spend is in
// flight; a credited purchase is paid; anything but a gift purchase is not
// found.
func TestCancelGammaGiftIntent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()
		networkId := giftGammaTestNetwork(t, ctx)
		now := server.NowUtc()

		_, open := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, now)
		if !CreateGammaCharge(ctx, open, now) {
			t.Fatal("no charge")
		}
		if outcome := CancelGammaGiftIntent(ctx, open.IntentId, now); outcome != GammaGiftCancelVoided {
			t.Fatalf("cancel: %s", outcome)
		}
		if state := giftGammaTestChargeState(ctx, open.IntentId); state != GammaChargeVoided {
			t.Fatalf("the charge is %s", state)
		}
		if outcome := CancelGammaGiftIntent(ctx, open.IntentId, now); outcome != GammaGiftCancelClosed {
			t.Fatalf("cancel again: %s", outcome)
		}

		_, flying := giftGammaTestOrder(t, ctx, networkId, 1, GiftDeliveryMe, now)
		if !CreateGammaCharge(ctx, flying, now) {
			t.Fatal("no charge")
		}
		giftGammaTestBatch(t, ctx, flying)
		if outcome := CancelGammaGiftIntent(ctx, flying.IntentId, now); outcome != GammaGiftCancelInFlight {
			t.Fatalf("cancel in flight: %s", outcome)
		}
		if GetGammaPaymentIntent(ctx, flying.IntentId).VoidedAt != nil {
			t.Fatal("a purchase in flight was voided")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			MarkGammaPaymentIntentCreditedInTx(tx, ctx, flying.IntentId, now)
		})
		if outcome := CancelGammaGiftIntent(ctx, flying.IntentId, now); outcome != GammaGiftCancelCredited {
			t.Fatalf("cancel credited: %s", outcome)
		}

		data := giftGammaTestDataIntent(t, ctx, networkId, now)
		if outcome := CancelGammaGiftIntent(ctx, data.IntentId, now); outcome != GammaGiftCancelNotFound {
			t.Fatalf("cancel a data purchase: %s", outcome)
		}
		if GetGammaPaymentIntent(ctx, data.IntentId).VoidedAt != nil {
			t.Fatal("a data purchase was voided")
		}
		if outcome := CancelGammaGiftIntent(ctx, server.NewId(), now); outcome != GammaGiftCancelNotFound {
			t.Fatalf("cancel nothing: %s", outcome)
		}
	})
}

// A pending order whose Gamma purchase was voided never emails its friends,
// so it no longer counts toward the buyer's daily friend emails; for a new
// gift purchase of the same network, neither do the orders of the open gift
// purchases it voids, unless their spend is in flight.
func TestGiftFriendEmailCountLeavesOutVoidedGammaOrders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetGiftConfig(Testing_GiftConfig())()
		networkId := giftGammaTestNetwork(t, ctx)
		otherNetworkId := giftGammaTestNetwork(t, ctx)
		now := server.NowUtc()
		since := now.Add(-GiftFriendEmailWindow)
		count := func(replacing *server.Id) int {
			return GetGiftFriendEmailCountReplacing(ctx, "giver@example.invalid", since, replacing)
		}

		_, open := giftGammaTestOrder(t, ctx, networkId, 5, GiftDeliveryThem, now)
		giftGammaTestOrder(t, ctx, otherNetworkId, 3, GiftDeliveryThem, now)
		if got := GetGiftFriendEmailCount(ctx, "Giver@Example.invalid", since); got != 8 {
			t.Fatalf("count %d, want 8", got)
		}
		// the network's new purchase will void its open one
		if got := count(&networkId); got != 3 {
			t.Fatalf("count replacing %d, want 3", got)
		}
		// not while its spend is in flight
		if !CreateGammaCharge(ctx, open, now) {
			t.Fatal("no charge")
		}
		giftGammaTestBatch(t, ctx, open)
		if got := count(&networkId); got != 8 {
			t.Fatalf("count replacing beside a spend in flight %d, want 8", got)
		}
		giftGammaTestExec(ctx, `UPDATE gamma_charge SET batch_id = NULL WHERE intent_id = $1`, open.IntentId)

		// voided: never counted
		if outcome := CancelGammaGiftIntent(ctx, open.IntentId, now); outcome != GammaGiftCancelVoided {
			t.Fatalf("cancel: %s", outcome)
		}
		if got := count(nil); got != 3 {
			t.Fatalf("count after the cancel %d, want 3", got)
		}
	})
}
