package controller

// Database-backed tests of a coin order's life after its credit
// (gift_crypto_controller.go, model.UnlinkCryptoPaymentIntents,
// model/gift_refund_model.go): the 30-day retention unlink forgets the
// invoice in the order and its codes, which still redeem; a payment that left
// the chain and came back is credited again with the order's own codes and
// redeemed years restored, unless the order was refunded; and an operator's
// refund refunds only the unredeemed codes. Needs the test database
// (server.DefaultTestEnv).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// giftCryptoTestOrder is a gift order paid with xmr: the order, its invoice
// and the receipt that paid it.
type giftCryptoTestOrder struct {
	order   *model.GiftOrder
	intent  *model.CryptoPaymentIntent
	receipt *model.CryptoPaymentReceipt
}

// giftCryptoTestPaidOrder stores an order of quantity gifts, every card to the
// buyer, and its xmr invoice (pool address index), and credits the invoice
// with a receipt of the full amount at the given confirmations.
func giftCryptoTestPaidOrder(
	t testing.TB,
	ctx context.Context,
	clientSession *session.ClientSession,
	quantity int,
	index int64,
	txId string,
	confirmations int,
) *giftCryptoTestOrder {
	return giftCryptoTestPay(t, ctx, clientSession, giftCryptoTestOrderOf(quantity, model.GiftDeliveryMe, time.Time{}), index, txId, confirmations)
}

// An order of quantity gifts paid in coins; delivery "them" sends each friend
// their card on sendOn in Chicago.
func giftCryptoTestOrderOf(quantity int, delivery string, sendOn time.Time) *model.GiftOrder {
	unitPriceCents := model.Gift().UnitPriceCents(quantity)
	order := &model.GiftOrder{
		Quantity:       quantity,
		UnitPriceCents: unitPriceCents,
		TotalCents:     unitPriceCents * int64(quantity),
		Rail:           model.GiftRailCrypto,
		Delivery:       delivery,
		BuyerEmail:     "buyer@synthetic.example",
		Language:       "en",
	}
	if delivery == model.GiftDeliveryThem {
		order.SendOn = &sendOn
		order.SendTimezone = "America/Chicago"
		for i := range quantity {
			order.Recipients = append(order.Recipients, fmt.Sprintf("friend%d@example.invalid", i))
		}
	}
	return order
}

// giftCryptoTestPay stores the order and its xmr invoice (pool address index),
// and credits the invoice with a receipt of the full amount at the given
// confirmations.
func giftCryptoTestPay(
	t testing.TB,
	ctx context.Context,
	clientSession *session.ClientSession,
	order *model.GiftOrder,
	index int64,
	txId string,
	confirmations int,
) *giftCryptoTestOrder {
	quantity := order.Quantity
	connect.AssertEqual(t, model.CreateGiftOrder(ctx, order), nil)
	intent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, nil, giftCryptoItem, float64(order.TotalCents)/100, index)
	connect.AssertEqual(t, model.SetGiftOrderCryptoIntentId(ctx, order.GiftOrderId, intent.IntentId), nil)
	receipt := cryptoDbReceipt(t, ctx, intent, txId, intent.AmountAtomic, confirmations)
	credited, err := cryptoCreditIntent(clientSession, intent, &cryptoCredit{
		usdNanoCents: intent.PriceUsdNanoCents,
		receipts:     []model.CryptoPaymentReceiptKey{receipt.Key()},
		receiptUsd:   map[model.CryptoPaymentReceiptKey]model.NanoCents{receipt.Key(): intent.PriceUsdNanoCents},
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, credited, true)
	connect.AssertEqual(t, len(model.GetGiftOrderCodes(ctx, order.GiftOrderId)), quantity)
	return &giftCryptoTestOrder{
		order:   order,
		intent:  model.GetCryptoPaymentIntent(ctx, intent.IntentId),
		receipt: receipt,
	}
}

// giftCryptoTestRedeem redeems a code into a new friend's network through
// the signed-in redeem route, and returns the network, which is Pro.
func giftCryptoTestRedeem(t testing.TB, ctx context.Context, secret string) server.Id {
	networkId := server.NewId()
	userId := server.NewId()
	// the id's random tail: its head is its time, shared by ids made together
	model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-friend-%s", giftCryptoTestIdTail(networkId)), userId)
	clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "friend", false, false))
	result, err := RedeemBalanceCode(RedeemBalanceCodeArgs{Secret: secret}, clientSession)
	if err != nil || result.Error != nil || result.Pro == nil {
		t.Fatalf("redeem: %+v %v", result, err)
	}
	if !model.IsPro(ctx, &networkId) {
		t.Fatal("the friend is not Pro")
	}
	return networkId
}

// The last 12 characters of an id, its random part.
func giftCryptoTestIdTail(id server.Id) string {
	text := id.String()
	return text[len(text)-12:]
}

// A code's times, as the 30-day unlink leaves them.
type giftCryptoTestTimes struct {
	createTime time.Time
	startTime  time.Time
	endTime    time.Time
	cancelTime *time.Time
}

// The times of every code of the order, by code.
func giftCryptoTestCodeTimes(t testing.TB, ctx context.Context, giftOrderId server.Id) map[server.Id]*giftCryptoTestTimes {
	t.Helper()
	times := map[server.Id]*giftCryptoTestTimes{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT balance_code_id, create_time, start_time, end_time, cancel_time FROM transfer_balance_code WHERE gift_order_id = $1`,
			giftOrderId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceCodeId server.Id
				codeTimes := &giftCryptoTestTimes{}
				server.Raise(result.Scan(&balanceCodeId, &codeTimes.createTime, &codeTimes.startTime, &codeTimes.endTime, &codeTimes.cancelTime))
				times[balanceCodeId] = codeTimes
			}
		})
	})
	return times
}

// Prints a paid order's cards, a synthetic PDF for each part, and owes its
// emails, as the render task does.
func giftCryptoTestPrint(t testing.TB, ctx context.Context, giftOrderId server.Id) {
	t.Helper()
	_, parts := giftOrderCardDocuments(model.GetGiftOrder(ctx, giftOrderId), model.GetGiftOrderCodes(ctx, giftOrderId))
	for _, part := range parts {
		if err := model.PutGiftOrderPdf(ctx, giftOrderId, part, []byte("%PDF-1.7 "+part)); err != nil {
			t.Fatal(err)
		}
	}
	giftDeliveryTestPost(t, ctx, giftOrderId)
}

// The recipients whose friend's email the order owes now: unsent and not given
// up, under any key.
func giftCryptoTestOwedFriendEmails(t testing.TB, ctx context.Context, giftOrderId server.Id) []int {
	t.Helper()
	owed := []int{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT (template_json ->> 'recipient_index')::int
				FROM account_message_outbox
				WHERE
					template_name = $1 AND
					template_json ->> 'gift_order_id' = $2 AND
					sent_time IS NULL AND
					abandon_time IS NULL
				ORDER BY 1
			`,
			giftCardFriendTemplateName,
			giftOrderId.String(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var recipientIndex int
				server.Raise(result.Scan(&recipientIndex))
				owed = append(owed, recipientIndex)
			}
		})
	})
	return owed
}

// The friends' emails the sender sent so far, by recipient, with the code each
// carried.
func giftCryptoTestFriendSends(sender *outboxTestSender) map[int][]string {
	sends := map[int][]string{}
	for _, send := range sender.sent() {
		if friend, ok := send.template.(*GiftCardFriendTemplate); ok {
			sends[friend.RecipientIndex] = append(sends[friend.RecipientIndex], friend.Code())
		}
	}
	return sends
}

// The codes' states, in code order.
func giftCryptoTestVoided(ctx context.Context, giftOrderId server.Id) []bool {
	voided := []bool{}
	for _, code := range model.GetGiftOrderCodes(ctx, giftOrderId) {
		voided = append(voided, code.Voided)
	}
	return voided
}

// The 30-day retention unlink forgets a paid gift order's invoice everywhere:
// the order loses the invoice's id and takes a fresh purchase event base, and
// its codes move under it and lose their purchase record and email. The order
// still finds its codes, and they still redeem. Nothing else of the order ties
// it to the invoice: its times and its codes' made and voided times become
// their day (a void code stays void; its listing, checks and redeem windows
// read the same), it loses its referrer (its codes keep theirs), and its
// buyer's email is blanked, unless a friend's email is still owed, whose
// reply-to it is, until none is. An order never paid loses the invoice's id
// too. A late payment to an unlinked gift invoice has nowhere to go.
func TestGiftCryptoOrderUnlink(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		// a signed-in giver's order, every card to the buyer
		referrerNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, referrerNetworkId, fmt.Sprintf("synthetic-giver-%s", giftCryptoTestIdTail(referrerNetworkId)), server.NewId())
		paidOrder := giftCryptoTestOrderOf(3, model.GiftDeliveryMe, time.Time{})
		paidOrder.ReferrerNetworkId = &referrerNetworkId
		paid := giftCryptoTestPay(t, ctx, taskSession, paidOrder, 921, "tx-gift-unlink", 2)
		invoiceBase := model.CryptoPaymentPurchaseEventId(paid.intent.IntentId)
		// an order whose friends get their cards in 40 days
		owing := giftCryptoTestPay(t, ctx, taskSession, giftCryptoTestOrderOf(2, model.GiftDeliveryThem, giftDeliveryTestDate(40)), 923, "tx-gift-unlink-owing", 2)
		giftCryptoTestPrint(t, ctx, owing.order.GiftOrderId)

		unitPriceCents := model.Gift().UnitPriceCents(1)
		pending := &model.GiftOrder{
			Quantity:       1,
			UnitPriceCents: unitPriceCents,
			TotalCents:     unitPriceCents,
			Rail:           model.GiftRailCrypto,
			Delivery:       model.GiftDeliveryMe,
			BuyerEmail:     "other@synthetic.example",
			Language:       "en",
		}
		connect.AssertEqual(t, model.CreateGiftOrder(ctx, pending), nil)
		pendingIntent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, nil, giftCryptoItem, float64(unitPriceCents)/100, 922)
		connect.AssertEqual(t, model.SetGiftOrderCryptoIntentId(ctx, pending.GiftOrderId, pendingIntent.IntentId), nil)

		// within the window, nothing is unlinked
		result, err := RunCryptoPaymentUnlink(taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 0)
		connect.AssertEqual(t, *model.GetGiftOrderIdForCryptoIntent(ctx, paid.intent.IntentId), paid.order.GiftOrderId)

		// the paid order's third code was voided, to the microsecond; what
		// reads its codes, as it reads them before the unlink
		paidCodes := model.GetGiftOrderCodes(ctx, paid.order.GiftOrderId)
		cryptoDbExec(ctx, `UPDATE transfer_balance_code SET cancel_time = $2 WHERE balance_code_id = $1`, paidCodes[2].BalanceCodeId, server.NowUtc())
		timesBefore := giftCryptoTestCodeTimes(t, ctx, paid.order.GiftOrderId)
		listedBefore := model.GetGiftOrderCodes(ctx, paid.order.GiftOrderId)
		checkedBefore := []*GiftCheckCodeResult{}
		for _, code := range listedBefore {
			checked, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: code.Secret}, taskSession)
			connect.AssertEqual(t, err, nil)
			checkedBefore = append(checkedBefore, checked)
		}
		connect.AssertEqual(t, checkedBefore[2].Status, model.GiftCodeStatusVoided)

		for _, intentId := range []server.Id{paid.intent.IntentId, owing.intent.IntentId} {
			cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET credited_at = $2 WHERE intent_id = $1`, intentId, server.NowUtc().Add(-721*time.Hour))
		}
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET create_time = $2 WHERE intent_id = $1`, pendingIntent.IntentId, server.NowUtc().Add(-721*time.Hour))
		result, err = RunCryptoPaymentUnlink(taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 3)

		// the invoice is forgotten by the order...
		unlinkedIntent := model.GetCryptoPaymentIntent(ctx, paid.intent.IntentId)
		connect.AssertEqual(t, unlinkedIntent.UnlinkedAt != nil, true)
		connect.AssertEqual(t, unlinkedIntent.BalanceCodeId, (*server.Id)(nil))
		connect.AssertEqual(t, model.GetGiftOrderIdForCryptoIntent(ctx, paid.intent.IntentId), (*server.Id)(nil))
		order := model.GetGiftOrder(ctx, paid.order.GiftOrderId)
		connect.AssertEqual(t, order.CryptoIntentId, (*server.Id)(nil))
		connect.AssertEqual(t, order.Status, model.GiftOrderStatusPaid)
		var purchaseEventBase string
		var namingCodes int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT purchase_event_base FROM gift_order WHERE gift_order_id = $1`, paid.order.GiftOrderId).Scan(&purchaseEventBase))
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM transfer_balance_code WHERE purchase_event_id = $1 OR starts_with(purchase_event_id, $1 || '/')`,
				invoiceBase,
			).Scan(&namingCodes))
		})
		connect.AssertEqual(t, strings.HasPrefix(purchaseEventBase, "crypto/unlinked/"), true)
		connect.AssertEqual(t, strings.Contains(purchaseEventBase, paid.intent.IntentId.String()), false)
		// ...and by every code
		connect.AssertEqual(t, namingCodes, 0)
		codes := model.GetGiftOrderCodes(ctx, paid.order.GiftOrderId)
		connect.AssertEqual(t, len(codes), 3)
		for n, code := range codes {
			balanceCode, err := model.GetBalanceCode(ctx, code.BalanceCodeId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, balanceCode.PurchaseEventId, fmt.Sprintf("%s/gift/%d", purchaseEventBase, n))
			connect.AssertEqual(t, balanceCode.PurchaseRecord, "")
			connect.AssertEqual(t, balanceCode.PurchaseEmail, "")
			connect.AssertEqual(t, code.Voided, n == 2)
		}

		// the codes were made and voided at their day's start: a void code
		// stays void, and their redeem windows stay as they were
		dayOf := func(at time.Time) time.Time {
			return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
		}
		timesAfter := giftCryptoTestCodeTimes(t, ctx, paid.order.GiftOrderId)
		connect.AssertEqual(t, len(timesAfter), 3)
		for balanceCodeId, before := range timesBefore {
			after := timesAfter[balanceCodeId]
			connect.AssertEqual(t, after.createTime, dayOf(before.createTime))
			connect.AssertEqual(t, after.startTime, before.startTime)
			connect.AssertEqual(t, after.endTime, before.endTime)
			if before.cancelTime == nil {
				connect.AssertEqual(t, after.cancelTime, (*time.Time)(nil))
			} else {
				connect.AssertEqual(t, after.cancelTime != nil, true)
				connect.AssertEqual(t, *after.cancelTime, dayOf(*before.cancelTime))
			}
		}
		connect.AssertEqual(t, timesAfter[paidCodes[2].BalanceCodeId].cancelTime != nil, true)
		// nothing that reads them changed: the order's listing, and each code's
		// check, its status and redeem-by
		connect.AssertEqual(t, model.GetGiftOrderCodes(ctx, paid.order.GiftOrderId), listedBefore)
		for n, code := range listedBefore {
			checked, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: code.Secret}, taskSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, checked, checkedBefore[n])
		}
		connect.AssertEqual(t, model.GetGiftOrder(ctx, pending.GiftOrderId).CryptoIntentId, (*server.Id)(nil))

		// the order's times are their day, it has no referrer while its codes
		// keep it, and its buyer's email is gone, as is the unpaid order's
		midnight := func(at time.Time) bool {
			return at.Equal(time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location()))
		}
		connect.AssertEqual(t, midnight(order.CreateTime), true)
		connect.AssertEqual(t, midnight(*order.PaidTime), true)
		connect.AssertEqual(t, order.ReferrerNetworkId, (*server.Id)(nil))
		var referredCodes int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM transfer_balance_code WHERE gift_order_id = $1 AND referrer_network_id = $2`,
				paid.order.GiftOrderId,
				referrerNetworkId,
			).Scan(&referredCodes))
		})
		connect.AssertEqual(t, referredCodes, 3)
		connect.AssertEqual(t, order.BuyerEmail, "")
		connect.AssertEqual(t, model.GetGiftOrder(ctx, pending.GiftOrderId).BuyerEmail, "")
		// the order whose friends' emails are owed keeps its buyer's address,
		// their reply-to, until they went out
		owingId := owing.order.GiftOrderId
		connect.AssertEqual(t, model.GetGiftOrder(ctx, owingId).CryptoIntentId, (*server.Id)(nil))
		connect.AssertEqual(t, model.GetGiftOrder(ctx, owingId).BuyerEmail, "buyer@synthetic.example")
		connect.AssertEqual(t, model.ClearUnlinkedGiftOrderBuyerEmail(ctx, owingId), false)
		giftRetentionTestSent(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(owingId, 0), server.NowUtc())
		connect.AssertEqual(t, model.ClearUnlinkedGiftOrderBuyerEmail(ctx, owingId), false)
		giftRetentionTestSent(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(owingId, 1), server.NowUtc())
		connect.AssertEqual(t, model.ClearUnlinkedGiftOrderBuyerEmail(ctx, owingId), true)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, owingId).BuyerEmail, "")
		connect.AssertEqual(t, model.ClearUnlinkedGiftOrderBuyerEmail(ctx, owingId), false)

		// the codes still redeem, and check; the void one does not redeem
		friendNetworkId := giftCryptoTestRedeem(t, ctx, codes[0].Secret)
		checked, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: codes[1].Secret}, taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusOk)
		connect.AssertEqual(t, model.GetGiftOrderCodes(ctx, paid.order.GiftOrderId)[0].Redeemed, true)
		friendSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(friendNetworkId, server.NewId(), "friend", false, false))
		voidRedeem, err := RedeemBalanceCode(RedeemBalanceCodeArgs{Secret: codes[2].Secret}, friendSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, voidRedeem.Error != nil, true)
		// and the friend's list of redeemed codes shows the year, from its
		// redemption
		redeemedCodes, err := model.FetchNetworkRedeemedBalanceCodes(friendSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(redeemedCodes), 1)
		connect.AssertEqual(t, redeemedCodes[0].BalanceCodeId, codes[0].BalanceCodeId)
		connect.AssertEqual(t, redeemedCodes[0].Kind, model.BalanceCodeKindProGift)
		connect.AssertEqual(t, redeemedCodes[0].EndTime.After(server.NowUtc().AddDate(0, 11, 0)), true)

		// a late payment to an unlinked gift invoice is held for an operator
		connect.AssertEqual(t, cryptoIntentUndeliverable(model.GetCryptoPaymentIntent(ctx, pendingIntent.IntentId)), true)
		// and an operator's credit of one finds no order to fulfil
		credited, err := cryptoCreditIntent(taskSession, unlinkedIntent, &cryptoCredit{usdNanoCents: unlinkedIntent.PriceUsdNanoCents})
		connect.AssertEqual(t, credited, false)
		connect.AssertEqual(t, err != nil, true)

		// a second pass finds nothing left
		result, err = RunCryptoPaymentUnlink(taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 0)
	})
}

// A credited payment that leaves the chain: the reconcile's orphan leg voids
// the order's unredeemed codes, ends its redeemed codes' years (two of them,
// in one transaction), and takes the credit back; the order stays paid. A
// friend's email that comes due while its code is void is withdrawn. When the
// payment is credited again, the same codes are restored and redeem, both
// years come back, and each withdrawn friend's email is owed again, once; one
// sent before is not. A refunded order's codes are never restored: its credit
// again fails, and its codes stay void.
func TestGiftCryptoOrphanAndRecredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		moneroReconcileHasCredentials = func() bool { return true }
		giftPurchaseTestConfig(t)
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		// the refund destination's screen, without OFAC's table
		cryptoTestSanctions(t, map[string]bool{}, nil)
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		// two orders paid, final, and unknown to the wallet: the transactions
		// left the chain. The kept one sends each friend their card on a date
		sendOn := giftDeliveryTestDate(40)
		friendDue := giftDeliveryTestChicagoMorning(t, sendOn)
		kept := giftCryptoTestPay(t, ctx, taskSession, giftCryptoTestOrderOf(4, model.GiftDeliveryThem, sendOn), 911, "tx-gift-orphan-kept", 12)
		refunded := giftCryptoTestPaidOrder(t, ctx, taskSession, 2, 912, "tx-gift-orphan-refunded", 12)
		for _, paid := range []*giftCryptoTestOrder{kept, refunded} {
			model.MarkCryptoPaymentReceiptsFinalInConn(nil, ctx, model.CryptoAssetXmr, []model.CryptoPaymentReceiptKey{paid.receipt.Key()}, server.NowUtc())
		}
		// the kept order's cards are printed and its emails owed; its first two
		// friends redeemed, and its third friend's email went out early
		giftCryptoTestPrint(t, ctx, kept.order.GiftOrderId)
		keptCodes := model.GetGiftOrderCodes(ctx, kept.order.GiftOrderId)
		friendNetworkIds := []server.Id{
			giftCryptoTestRedeem(t, ctx, keptCodes[0].Secret),
			giftCryptoTestRedeem(t, ctx, keptCodes[1].Secret),
		}
		giftRetentionTestSent(t, ctx, giftCardFriendTemplateName, giftFriendEmailKey(kept.order.GiftOrderId, 2), server.NowUtc())
		// the other refunded first: every code, none redeemed
		payout := "5" + fmt.Sprintf("%094d", 7)
		refundResult, err := CryptoOperatorRefund(taskSession, refunded.intent.IntentId, payout)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refundResult.Gift.Refunded, 2)
		connect.AssertEqual(t, refundResult.Gift.RefundAtomic, refundResult.ReceivedAtomic)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, refunded.order.GiftOrderId).Status, model.GiftOrderStatusRefunded)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, refunded.order.GiftOrderId), []bool{true, true})

		model.SetCryptoScanCursorSuccessInConn(nil, ctx, model.CryptoAssetXmr, 1000, 1000, 1000, 1000, server.NowUtc())
		_, err = RunPaymentReconciliationWithOptions(
			reconcileTestSession(t, ctx),
			&PaymentReconcileRunOptions{Stores: []string{model.SubscriptionMarketMonero}},
		)
		connect.AssertEqual(t, err, nil)

		// both credits were taken back; the paid order's unredeemed codes are
		// void, its redeemed codes' years ended, and it stays paid
		for _, paid := range []*giftCryptoTestOrder{kept, refunded} {
			connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, paid.intent.IntentId).CreditedAt, (*time.Time)(nil))
		}
		connect.AssertEqual(t, model.GetGiftOrder(ctx, kept.order.GiftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, kept.order.GiftOrderId), []bool{false, false, true, true})
		for _, friendNetworkId := range friendNetworkIds {
			connect.AssertEqual(t, model.IsPro(ctx, &friendNetworkId), false)
		}
		checked, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: keptCodes[2].Secret}, taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusVoided)

		// the friends' date comes while the codes are void: the fourth
		// friend's email is withdrawn (the third's went out before)
		sender := newOutboxTestSender()
		deliverAccountMessagesAt(ctx, sender, friendDue.Add(time.Minute))
		connect.AssertEqual(t, giftCryptoTestFriendSends(sender), map[int][]string{
			0: {giftCardCodeGrouped(keptCodes[0].Secret)},
			1: {giftCardCodeGrouped(keptCodes[1].Secret)},
		})
		withdrawn := model.GetAccountMessageByKey(ctx, giftCardFriendTemplateName, giftFriendEmailKey(kept.order.GiftOrderId, 3))
		connect.AssertEqual(t, withdrawn.SentTime, (*time.Time)(nil))
		connect.AssertEqual(t, withdrawn.AbandonTime != nil, true)
		connect.AssertEqual(t, giftCryptoTestOwedFriendEmails(t, ctx, kept.order.GiftOrderId), []int{})

		// the payment turns out to be on the chain after all: credited again,
		// the same codes are good again, and the redeemed codes' years are back
		credited, err := CryptoOperatorCredit(taskSession, kept.intent.IntentId, false)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		recredited := model.GetCryptoPaymentIntent(ctx, kept.intent.IntentId)
		connect.AssertEqual(t, recredited.CreditedAt != nil, true)
		connect.AssertEqual(t, *recredited.BalanceCodeId, keptCodes[0].BalanceCodeId)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, kept.order.GiftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, kept.order.GiftOrderId), []bool{false, false, false, false})
		for _, friendNetworkId := range friendNetworkIds {
			connect.AssertEqual(t, model.IsPro(ctx, &friendNetworkId), true)
		}
		for n, code := range model.GetGiftOrderCodes(ctx, kept.order.GiftOrderId) {
			// the same codes, under the same purchase events
			connect.AssertEqual(t, code.BalanceCodeId, keptCodes[n].BalanceCodeId)
			connect.AssertEqual(t, code.Secret, keptCodes[n].Secret)
		}
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, kept.order.GiftOrderId), 4)
		connect.AssertEqual(t, model.GetCryptoPaymentReceiptsForIntent(ctx, kept.intent.IntentId)[0].State, model.CryptoReceiptStateCredited)
		var voidRows int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM gift_code_void WHERE gift_order_id = $1`, kept.order.GiftOrderId).Scan(&voidRows))
		})
		connect.AssertEqual(t, voidRows, 0)
		// the withdrawn email is owed again, once, and sends its friend's code
		connect.AssertEqual(t, giftCryptoTestOwedFriendEmails(t, ctx, kept.order.GiftOrderId), []int{3})
		deliverAccountMessagesAt(ctx, sender, friendDue.Add(2*time.Minute))
		connect.AssertEqual(t, giftCryptoTestFriendSends(sender), map[int][]string{
			0: {giftCardCodeGrouped(keptCodes[0].Secret)},
			1: {giftCardCodeGrouped(keptCodes[1].Secret)},
			3: {giftCardCodeGrouped(keptCodes[3].Secret)},
		})
		connect.AssertEqual(t, giftCryptoTestOwedFriendEmails(t, ctx, kept.order.GiftOrderId), []int{})
		giftCryptoTestRedeem(t, ctx, keptCodes[2].Secret)
		checked, err = GiftCheckCode(&GiftCheckCodeArgs{Secret: keptCodes[3].Secret}, taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusOk)

		// a refund's voids are final: the refunded order's credit again fails,
		// and nothing of it changes
		credited, err = CryptoOperatorCredit(taskSession, refunded.intent.IntentId, false)
		connect.AssertEqual(t, credited, false)
		connect.AssertEqual(t, err != nil, true)
		connect.AssertEqual(t, strings.Contains(err.Error(), refunded.order.GiftOrderId.String()), false)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, refunded.intent.IntentId).CreditedAt, (*time.Time)(nil))
		connect.AssertEqual(t, model.GetGiftOrder(ctx, refunded.order.GiftOrderId).Status, model.GiftOrderStatusRefunded)
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, refunded.order.GiftOrderId), []bool{true, true})
		refundedCodes := model.GetGiftOrderCodes(ctx, refunded.order.GiftOrderId)
		checked, err = GiftCheckCode(&GiftCheckCodeArgs{Secret: refundedCodes[0].Secret}, taskSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusVoided)
	})
}

// An operator's refund of a gift order's invoice refunds only its unredeemed
// codes (decision 16): it voids them, never ends a redeemed code's year, and
// says what to send back, the refunded codes' share of what was received and
// of its credited value. A second refund is refused, and so is one of an order
// with nothing refundable left.
func TestGiftCryptoOperatorRefund(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		cryptoTestSanctions(t, map[string]bool{}, nil)
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		payout := "5" + fmt.Sprintf("%094d", 7)

		paid := giftCryptoTestPaidOrder(t, ctx, taskSession, 4, 931, "tx-gift-refund-part", 12)
		codes := model.GetGiftOrderCodes(ctx, paid.order.GiftOrderId)
		friendNetworkId := giftCryptoTestRedeem(t, ctx, codes[0].Secret)

		result, err := CryptoOperatorRefund(taskSession, paid.intent.IntentId, payout)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.EndedNetworkIds, []server.Id(nil))
		connect.AssertEqual(t, result.ReceivedAtomic, paid.intent.AmountAtomic)
		connect.AssertEqual(t, result.Gift.Quantity, 4)
		connect.AssertEqual(t, result.Gift.Refunded, 3)
		connect.AssertEqual(t, result.Gift.Redeemed, 1)
		connect.AssertEqual(t, result.Gift.AlreadyVoid, 0)
		connect.AssertEqual(t, result.Gift.RefundAtomic, paid.intent.AmountAtomic*3/4)
		connect.AssertEqual(t, result.Gift.Refund, cryptoFormatAtomic(paid.intent.AmountAtomic*3/4, cryptoAssetDecimals(model.CryptoAssetXmr)))
		connect.AssertEqual(t, result.Gift.RefundUsd, model.NanoCentsToUsd(*paid.intent.CreditedUsdNanoCents*3/4))
		// the unredeemed codes are void, the redeemed one's year continues
		connect.AssertEqual(t, giftCryptoTestVoided(ctx, paid.order.GiftOrderId), []bool{false, true, true, true})
		connect.AssertEqual(t, model.GetGiftOrderCodes(ctx, paid.order.GiftOrderId)[0].Redeemed, true)
		connect.AssertEqual(t, model.IsPro(ctx, &friendNetworkId), true)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, paid.order.GiftOrderId).Status, model.GiftOrderStatusRefunded)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, paid.intent.IntentId).RefundedAt != nil, true)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketMonero, model.PaymentReconcileActionRefunded, paid.intent.IntentId.String()),
			1,
		)

		// once only
		_, err = CryptoOperatorRefund(taskSession, paid.intent.IntentId, payout)
		connect.AssertEqual(t, err, errCryptoRefundRefused)

		// an order whose every code is redeemed has nothing to refund, and
		// records nothing
		redeemed := giftCryptoTestPaidOrder(t, ctx, taskSession, 1, 932, "tx-gift-refund-none", 12)
		redeemedNetworkId := giftCryptoTestRedeem(t, ctx, model.GetGiftOrderCodes(ctx, redeemed.order.GiftOrderId)[0].Secret)
		_, err = CryptoOperatorRefund(taskSession, redeemed.intent.IntentId, payout)
		connect.AssertEqual(t, errors.Is(err, errCryptoRefundGiftNothingRefundable), true)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, redeemed.intent.IntentId).RefundedAt, (*time.Time)(nil))
		connect.AssertEqual(t, model.GetGiftOrder(ctx, redeemed.order.GiftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, model.IsPro(ctx, &redeemedNetworkId), true)
	})
}
