package controller

// Database-backed tests of the 30-day retention unlink of a gift order's coin
// invoice that received a payment it was not credited for (owner, 2026-11-16:
// only unpaid gift invoices unlink; model.UnlinkCryptoPaymentIntents): such an
// invoice stays linked to its order past its 30 days, so the scan or an
// operator still credits it, and the order's codes are made and its cards
// delivered. Once it is credited it unlinks 30 days after the credit, as every
// credited invoice does; once it is voided or refunded, or no payment holds it
// any more, at the next run. A gift invoice never paid, and every network or
// data code invoice, paid or not, unlinks as before. Needs the test database
// (server.DefaultTestEnv).

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
	"github.com/urnetwork/server/task"
)

// When the tests' old invoices were made: 31 days ago, a day past
// unlink_after (cryptoTestConfig's 720h).
func giftUnlinkTestMadeAt() time.Time {
	return server.NowUtc().Add(-31 * 24 * time.Hour)
}

// Moves an invoice's making and its quote 31 days back.
func giftUnlinkTestAge(t testing.TB, ctx context.Context, intentId server.Id) *model.CryptoPaymentIntent {
	t.Helper()
	madeAt := giftUnlinkTestMadeAt()
	cryptoDbExec(
		ctx,
		`UPDATE crypto_payment_intent SET create_time = $2, quoted_at = $2, expires_at = $3 WHERE intent_id = $1`,
		intentId,
		madeAt,
		madeAt.Add(30*time.Minute),
	)
	return model.GetCryptoPaymentIntent(ctx, intentId)
}

// A gift order of quantity gifts, every card to the buyer, and its xmr
// invoice at the pool address index, never credited, made 31 days ago.
func giftUnlinkTestOrder(t testing.TB, ctx context.Context, quantity int, index int64) (*model.GiftOrder, *model.CryptoPaymentIntent) {
	t.Helper()
	order := giftCryptoTestOrderOf(quantity, model.GiftDeliveryMe, time.Time{})
	connect.AssertEqual(t, model.CreateGiftOrder(ctx, order), nil)
	intent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, nil, giftCryptoItem, float64(order.TotalCents)/100, index)
	connect.AssertEqual(t, model.SetGiftOrderCryptoIntentId(ctx, order.GiftOrderId, intent.IntentId), nil)
	return order, giftUnlinkTestAge(t, ctx, intent.IntentId)
}

// Records a receipt of amountAtomic to the invoice, first seen within its
// quote, in the given state and review reason.
func giftUnlinkTestReceipt(
	t testing.TB,
	ctx context.Context,
	intent *model.CryptoPaymentIntent,
	txId string,
	amountAtomic int64,
	state string,
	reviewReason string,
) *model.CryptoPaymentReceipt {
	t.Helper()
	confirmations := 2
	if state == model.CryptoReceiptStateSeen {
		confirmations = 0
	}
	receipt := cryptoTestReceipt(intent, txId, amountAtomic, confirmations, intent.QuotedAt.Add(10*time.Minute), state)
	receipt.ReviewReason = reviewReason
	inserted, stored := model.UpsertCryptoPaymentReceiptInConn(nil, ctx, receipt)
	connect.AssertEqual(t, inserted, true)
	return stored
}

// One retention unlink run; the invoices it unlinked.
func giftUnlinkTestRun(t testing.TB, taskSession *session.ClientSession) int {
	t.Helper()
	result, err := RunCryptoPaymentUnlink(taskSession)
	connect.AssertEqual(t, err, nil)
	return result.Unlinked
}

// Whether the invoice is still linked to its order. The two always agree: an
// invoice not unlinked is named by its order, and an unlinked one by no order.
func giftUnlinkTestLinked(t testing.TB, ctx context.Context, giftOrderId server.Id, intentId server.Id) bool {
	t.Helper()
	intent := model.GetCryptoPaymentIntent(ctx, intentId)
	linkedOrderId := model.GetGiftOrderIdForCryptoIntent(ctx, intentId)
	orderIntentId := model.GetGiftOrder(ctx, giftOrderId).CryptoIntentId
	if intent.UnlinkedAt == nil {
		if linkedOrderId == nil || *linkedOrderId != giftOrderId || orderIntentId == nil || *orderIntentId != intentId {
			t.Fatalf("invoice %s is not unlinked, but its order does not name it", intentId)
		}
		return true
	}
	if linkedOrderId != nil || orderIntentId != nil {
		t.Fatalf("invoice %s is unlinked, but its order still names it", intentId)
	}
	return false
}

// The order's pending card renders: one once a credit paid it.
func giftUnlinkTestRenders(t testing.TB, ctx context.Context, giftOrderId server.Id) int {
	t.Helper()
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM pending_task WHERE function_name = $1 AND run_once_key = $2`,
			task.NewTaskTarget(GiftRenderCards).TargetFunctionName(),
			giftRenderCardsRunOnce(giftOrderId).String(),
		).Scan(&count))
	})
	return count
}

// The order is paid by its invoice's credit: its codes are made under the
// invoice's purchase events, the invoice records the first as its grant, and
// the delivery of its cards is owed.
func giftUnlinkTestDelivered(t testing.TB, ctx context.Context, giftOrderId server.Id, intentId server.Id, quantity int) {
	t.Helper()
	order := model.GetGiftOrder(ctx, giftOrderId)
	connect.AssertEqual(t, order.Status, model.GiftOrderStatusPaid)
	connect.AssertEqual(t, order.PaidTime != nil, true)
	connect.AssertEqual(t, order.BuyerEmail, "buyer@synthetic.example")
	codes := model.GetGiftOrderCodes(ctx, giftOrderId)
	connect.AssertEqual(t, len(codes), quantity)
	for n, code := range codes {
		balanceCode, err := model.GetBalanceCode(ctx, code.BalanceCodeId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, balanceCode.PurchaseEventId, fmt.Sprintf("crypto/%s/gift/%d", intentId, n))
		connect.AssertEqual(t, code.Voided, false)
	}
	credited := model.GetCryptoPaymentIntent(ctx, intentId)
	connect.AssertEqual(t, credited.CreditedAt != nil, true)
	connect.AssertEqual(t, *credited.BalanceCodeId, codes[0].BalanceCodeId)
	connect.AssertEqual(t, giftUnlinkTestRenders(t, ctx, giftOrderId), 1)
}

// A gift invoice past its 30 days that received a payment it was not credited
// for stays linked to its order: one held for review (underpaid), and one
// still in the mempool, below its credit depth, that arrived after its quote.
// An operator's `crypto credit` of the first, and the scan's credit of the
// second once it is mined, make each order's codes and owe its cards. Then
// each unlinks 30 days after its credit, as any credited gift invoice does:
// the order forgets the invoice, and its codes move to a fresh base.
func TestGiftCryptoUnlinkKeepsAnInvoiceWithAPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetXmr)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetXmr: wallet})
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		// held for review: short of its quote when it expired
		heldOrder, held := giftUnlinkTestOrder(t, ctx, 3, 941)
		giftUnlinkTestReceipt(t, ctx, held, "tx-gift-unlink-underpaid", held.AmountAtomic*99/100, model.CryptoReceiptStateReview, model.CryptoReviewReasonUnderpaid)
		// received and not credited yet: late, and in the mempool when the
		// unlink runs
		receivedOrder, received := giftUnlinkTestOrder(t, ctx, 2, 942)
		wallet.setHeight(1000)
		wallet.pay("tx-gift-unlink-mempool", 942, uint64(received.AmountAtomic))
		scan, err := RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Error, "")
		connect.AssertEqual(t, scan.Credited, 0)
		receipts := model.GetCryptoPaymentReceiptsForIntent(ctx, received.IntentId)
		connect.AssertEqual(t, len(receipts), 1)
		connect.AssertEqual(t, receipts[0].State, model.CryptoReceiptStateSeen)

		// past 30 days, neither is unlinked, and a second run agrees
		for range 2 {
			connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 0)
			connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, heldOrder.GiftOrderId, held.IntentId), true)
			connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, receivedOrder.GiftOrderId, received.IntentId), true)
		}
		// the orders keep their buyer's address, which the delivery emails
		connect.AssertEqual(t, model.GetGiftOrder(ctx, heldOrder.GiftOrderId).BuyerEmail, "buyer@synthetic.example")
		connect.AssertEqual(t, model.GetGiftOrder(ctx, receivedOrder.GiftOrderId).BuyerEmail, "buyer@synthetic.example")
		connect.AssertEqual(t, model.GetGiftOrder(ctx, heldOrder.GiftOrderId).Status, model.GiftOrderStatusPending)

		// the operator credits the held payment: the order is paid and its
		// cards are owed
		credited, err := CryptoOperatorCredit(taskSession, held.IntentId, false)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, credited, true)
		giftUnlinkTestDelivered(t, ctx, heldOrder.GiftOrderId, held.IntentId, 3)

		// the payment in the mempool is mined, and the scan credits it
		wallet.mine("tx-gift-unlink-mempool", 1000)
		scan, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetXmr)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, scan.Error, "")
		connect.AssertEqual(t, scan.CreditErrored, 0)
		connect.AssertEqual(t, scan.Credited, 1)
		giftUnlinkTestDelivered(t, ctx, receivedOrder.GiftOrderId, received.IntentId, 2)

		// credited now, both keep the link for 30 days after the credit
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 0)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, heldOrder.GiftOrderId, held.IntentId), true)

		// and then unlink as any credited gift invoice does
		for _, intentId := range []server.Id{held.IntentId, received.IntentId} {
			cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET credited_at = $2 WHERE intent_id = $1`, intentId, server.NowUtc().Add(-721*time.Hour))
		}
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 2)
		for _, paid := range []struct {
			giftOrderId server.Id
			intentId    server.Id
			quantity    int
		}{
			{heldOrder.GiftOrderId, held.IntentId, 3},
			{receivedOrder.GiftOrderId, received.IntentId, 2},
		} {
			connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, paid.giftOrderId, paid.intentId), false)
			var purchaseEventBase string
			var namingCodes int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT purchase_event_base FROM gift_order WHERE gift_order_id = $1`, paid.giftOrderId).Scan(&purchaseEventBase))
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT count(*) FROM transfer_balance_code WHERE starts_with(purchase_event_id, $1)`,
					model.CryptoPaymentPurchaseEventId(paid.intentId),
				).Scan(&namingCodes))
			})
			connect.AssertEqual(t, strings.HasPrefix(purchaseEventBase, "crypto/unlinked/"), true)
			connect.AssertEqual(t, namingCodes, 0)
			codes := model.GetGiftOrderCodes(ctx, paid.giftOrderId)
			connect.AssertEqual(t, len(codes), paid.quantity)
			for n, code := range codes {
				balanceCode, err := model.GetBalanceCode(ctx, code.BalanceCodeId)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, balanceCode.PurchaseEventId, fmt.Sprintf("%s/gift/%d", purchaseEventBase, n))
			}
		}
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 0)
	})
}

// A gift invoice past its 30 days that never received a payment unlinks as
// before: its order forgets it and its buyer's address, and no code names
// it. So does one whose only receipts never counted (ignored) or left the
// chain before any credit (orphaned): neither the scan nor an operator's
// credit counts those. An operator's credit of an unlinked gift invoice is
// refused, as no credit can find its order.
func TestGiftCryptoUnlinkUnpaidInvoiceAsBefore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		unpaidOrder, unpaid := giftUnlinkTestOrder(t, ctx, 3, 951)
		uncountedOrder, uncounted := giftUnlinkTestOrder(t, ctx, 1, 952)
		giftUnlinkTestReceipt(t, ctx, uncounted, "tx-gift-unlink-locked", uncounted.AmountAtomic, model.CryptoReceiptStateIgnored, model.CryptoReviewReasonLocked)
		giftUnlinkTestReceipt(t, ctx, uncounted, "tx-gift-unlink-dropped", uncounted.AmountAtomic, model.CryptoReceiptStateOrphaned, "")
		// within its 30 days, an unpaid one stays
		recentOrder := giftCryptoTestOrderOf(2, model.GiftDeliveryMe, time.Time{})
		connect.AssertEqual(t, model.CreateGiftOrder(ctx, recentOrder), nil)
		recent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, nil, giftCryptoItem, float64(recentOrder.TotalCents)/100, 953)
		connect.AssertEqual(t, model.SetGiftOrderCryptoIntentId(ctx, recentOrder.GiftOrderId, recent.IntentId), nil)

		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 2)
		for _, pending := range []struct {
			giftOrderId server.Id
			intentId    server.Id
		}{
			{unpaidOrder.GiftOrderId, unpaid.IntentId},
			{uncountedOrder.GiftOrderId, uncounted.IntentId},
		} {
			connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, pending.giftOrderId, pending.intentId), false)
			order := model.GetGiftOrder(ctx, pending.giftOrderId)
			connect.AssertEqual(t, order.Status, model.GiftOrderStatusPending)
			connect.AssertEqual(t, order.BuyerEmail, "")
			var namingCodes int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT count(*) FROM transfer_balance_code WHERE starts_with(purchase_event_id, $1)`,
					model.CryptoPaymentPurchaseEventId(pending.intentId),
				).Scan(&namingCodes))
			})
			connect.AssertEqual(t, namingCodes, 0)
		}
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, recentOrder.GiftOrderId, recent.IntentId), true)

		// the receipts that held nothing cannot be credited, and the invoice
		// no longer knows its order
		_, err := CryptoOperatorCredit(taskSession, uncounted.IntentId, false)
		connect.AssertEqual(t, err != nil && strings.Contains(err.Error(), "unlinked from its gift order"), true)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, uncountedOrder.GiftOrderId).Status, model.GiftOrderStatusPending)

		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 0)
	})
}

// A held gift invoice unlinks at the next run once its payment is resolved:
// voided by an operator; its only payment gone from the chain before any
// credit; or refunded, here a refunded order whose payment then left the
// chain, which uncredits its invoice and holds its receipt for review. The
// same invoice, credited and then uncredited by its payment leaving the
// chain, but not refunded, stays held: an operator can credit it again.
func TestGiftCryptoUnlinkReleasesAResolvedPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		cryptoTestStandardRates(t)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		// the refund destination's screen, without OFAC's table
		cryptoTestSanctions(t, map[string]bool{}, nil)
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		voidedOrder, voided := giftUnlinkTestOrder(t, ctx, 2, 961)
		giftUnlinkTestReceipt(t, ctx, voided, "tx-gift-unlink-void", voided.AmountAtomic/2, model.CryptoReceiptStateReview, model.CryptoReviewReasonUnderpaid)
		goneOrder, gone := giftUnlinkTestOrder(t, ctx, 1, 962)
		goneReceipt := giftUnlinkTestReceipt(t, ctx, gone, "tx-gift-unlink-gone", gone.AmountAtomic, model.CryptoReceiptStateConfirmed, "")

		// two paid orders whose payments then left the chain (the reconcile's
		// orphan leg: the receipt held as orphaned, the credit taken back),
		// one of them refunded first
		refunded := giftCryptoTestPaidOrder(t, ctx, taskSession, 2, 963, "tx-gift-unlink-refunded", 2)
		payout := "5" + fmt.Sprintf("%094d", 9)
		refund, err := CryptoOperatorRefund(taskSession, refunded.intent.IntentId, payout)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refund.Gift.Refunded, 2)
		orphaned := giftCryptoTestPaidOrder(t, ctx, taskSession, 1, 964, "tx-gift-unlink-orphaned", 2)
		for _, paid := range []*giftCryptoTestOrder{refunded, orphaned} {
			intentId := paid.intent.IntentId
			server.Tx(ctx, func(tx server.PgTx) {
				model.SetCryptoPaymentReceiptsStateInTx(tx, ctx, &intentId, []model.CryptoPaymentReceiptKey{paid.receipt.Key()}, model.CryptoReceiptStateReview, model.CryptoReviewReasonOrphaned)
				connect.AssertEqual(t, model.UncreditCryptoPaymentIntentInTx(tx, ctx, intentId), true)
			})
			giftUnlinkTestAge(t, ctx, intentId)
		}
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, refunded.intent.IntentId).RefundedAt != nil, true)
		connect.AssertEqual(t, model.GetCryptoPaymentIntent(ctx, refunded.intent.IntentId).CreditedAt, (*time.Time)(nil))

		// the refunded one is resolved: unlinked at once; the rest are held
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 1)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, refunded.order.GiftOrderId, refunded.intent.IntentId), false)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, voidedOrder.GiftOrderId, voided.IntentId), true)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, goneOrder.GiftOrderId, gone.IntentId), true)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, orphaned.order.GiftOrderId, orphaned.intent.IntentId), true)

		// voided by an operator (the coins to go back by hand): the next run
		voidedNow, err := CryptoOperatorVoid(ctx, voided.IntentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, voidedNow, true)
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 1)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, voidedOrder.GiftOrderId, voided.IntentId), false)

		// its only payment dropped from the chain before any credit: the next
		// run
		goneIntentId := gone.IntentId
		model.SetCryptoPaymentReceiptsStateInConn(nil, ctx, &goneIntentId, []model.CryptoPaymentReceiptKey{goneReceipt.Key()}, model.CryptoReceiptStateOrphaned, "")
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 1)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, goneOrder.GiftOrderId, gone.IntentId), false)

		// the payment that left the chain after its credit, not refunded, is
		// still held
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 0)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, orphaned.order.GiftOrderId, orphaned.intent.IntentId), true)
	})
}

// A USDC gift invoice is held by a recorded unfulfilled Solana payment that
// names its reference: a payment sent without the reference, whose amount
// matches the invoice's quote (never credited by its amount alone, as a code
// invoice's), credited past 30 days with `crypto credit-usdc`, which needs the
// invoice still linked; and an underpayment, until the invoice is voided. A
// USDC gift invoice no record names unlinks as before.
func TestGiftCryptoUnlinkHoldsAUsdcInvoiceWithARecordedPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		solanaTestAmountSuffixes(t, 4401, 4402, 4403)
		pageSession := session.Testing_CreateClientSession(ctx, nil)
		taskSession := session.Testing_CreateClientSession(ctx, nil)

		startOrder := func(quantity int) (server.Id, *model.CryptoPaymentIntent) {
			giftPurchaseTestResetLimiters()
			args := &GiftCryptoIntentArgs{
				GiftPurchaseArgs: GiftPurchaseArgs{
					Quantity: quantity,
					Delivery: model.GiftDeliveryMe,
					Email:    "buyer@synthetic.example",
					Language: "en",
				},
				Asset: model.CryptoAssetUsdcSolana,
			}
			invoice, err := GiftCryptoIntent(args, pageSession)
			connect.AssertEqual(t, err, nil)
			if invoice.Error != nil {
				t.Fatalf("gift usdc invoice: %+v", invoice.Error)
			}
			intent := model.GetCryptoPaymentIntent(ctx, server.RequireParseId(invoice.IntentId))
			connect.AssertEqual(t, intent.SolanaReference != nil, true)
			return server.RequireParseId(invoice.OrderId), intent
		}
		memolessOrderId, memoless := startOrder(2)
		underpaidOrderId, underpaid := startOrder(1)
		unpaidOrderId, unpaid := startOrder(3)

		// sent by hand without the reference, the quote's exact amount, a
		// minute after the quote: recorded, its note naming the invoice's
		// Solana Pay intent
		signature := "sig-gift-unlink-memoless"
		payment := solanaMemolessPayment(signature, solanaReceiverAddresses[0], model.SolanaMicroToUsd(memoless.AmountAtomic), memoless.QuotedAt.Add(time.Minute))
		webhook, err := HeliusWebhook([]*SolanaTransaction{payment}, session.Testing_CreateClientSession(ctx, nil))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, webhook.Message, "No payment intent found for this network ID")
		recorded, err := model.GetUnfulfilledSolanaPayment(ctx, signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, recorded.Reason, model.SolanaUnfulfilledReasonNoIntent)
		connect.AssertEqual(t, recorded.MatchNote != nil && strings.Contains(*recorded.MatchNote, *memoless.SolanaReference), true)
		// short of the other's quote, with its reference
		expected := model.SolanaMicroToUsd(underpaid.AmountAtomic)
		connect.AssertEqual(t, model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature:       "sig-gift-unlink-underpaid",
			Reason:            model.SolanaUnfulfilledReasonUnderpaid,
			TokenAmountUsd:    expected - 5,
			ExpectedAmountUsd: &expected,
			PaymentReference:  underpaid.SolanaReference,
		}), nil)

		for _, intent := range []*model.CryptoPaymentIntent{memoless, underpaid, unpaid} {
			giftUnlinkTestAge(t, ctx, intent.IntentId)
		}
		// only the one no record names is unlinked
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 1)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, unpaidOrderId, unpaid.IntentId), false)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, memolessOrderId, memoless.IntentId), true)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, underpaidOrderId, underpaid.IntentId), true)

		// the owner-approved credit of the recorded payment, past 30 days,
		// delivers the order
		plan, err := CryptoOperatorPlanUsdcCredit(ctx, memoless.IntentId, signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, plan.State, CryptoUsdcCreditStateCredit)
		committed, err := CryptoOperatorCommitUsdcCredit(taskSession, memoless.IntentId, signature, plan.Digest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, committed.State, CryptoUsdcCreditStateCredited)
		order := model.GetGiftOrder(ctx, memolessOrderId)
		connect.AssertEqual(t, order.Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, len(model.GetGiftOrderCodes(ctx, memolessOrderId)), 2)
		connect.AssertEqual(t, giftUnlinkTestRenders(t, ctx, memolessOrderId), 1)
		_, err = model.GetUnfulfilledSolanaPayment(ctx, signature)
		connect.AssertNotEqual(t, err, nil)

		// the underpayment is given up: voided, the next run unlinks it; the
		// credited one keeps its link for 30 days after its credit
		voided, err := CryptoOperatorVoid(ctx, underpaid.IntentId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, voided, true)
		connect.AssertEqual(t, giftUnlinkTestRun(t, taskSession), 1)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, underpaidOrderId, underpaid.IntentId), false)
		connect.AssertEqual(t, giftUnlinkTestLinked(t, ctx, memolessOrderId, memoless.IntentId), true)
	})
}

// Network and data code invoices are unlinked at 30 days as before, paid or
// not: one held for review, one received and not yet credited, and one never
// paid, each to a network, and a data code held for review. Only a gift
// order's invoice waits for its payment.
func TestCryptoUnlinkNetworkInvoicesPaidOrNot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "cryptounlinkpaidornot")

		held := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, model.SolanaPlanYearly, 40, 971)
		held = giftUnlinkTestAge(t, ctx, held.IntentId)
		giftUnlinkTestReceipt(t, ctx, held, "tx-network-unlink-underpaid", held.AmountAtomic/2, model.CryptoReceiptStateReview, model.CryptoReviewReasonUnderpaid)
		received := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData1Tib, 3, 972)
		received = giftUnlinkTestAge(t, ctx, received.IntentId)
		giftUnlinkTestReceipt(t, ctx, received, "tx-network-unlink-confirmed", received.AmountAtomic, model.CryptoReceiptStateConfirmed, "")
		unpaid := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, &networkId, StripeItemData10Tib, 20, 973)
		unpaid = giftUnlinkTestAge(t, ctx, unpaid.IntentId)
		code := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, nil, StripeItemData1Tib, 3, 974)
		code = giftUnlinkTestAge(t, ctx, code.IntentId)
		giftUnlinkTestReceipt(t, ctx, code, "tx-code-unlink-underpaid", code.AmountAtomic/2, model.CryptoReceiptStateReview, model.CryptoReviewReasonUnderpaid)

		result, err := RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 4)
		for _, intent := range []*model.CryptoPaymentIntent{held, received, unpaid, code} {
			unlinked := model.GetCryptoPaymentIntent(ctx, intent.IntentId)
			connect.AssertEqual(t, unlinked.UnlinkedAt != nil, true)
			connect.AssertEqual(t, unlinked.NetworkId, (*server.Id)(nil))
			connect.AssertEqual(t, unlinked.CreditedAt, (*time.Time)(nil))
		}
		// a credit the rules would make for the held network invoice now has
		// nowhere to go, as before
		connect.AssertEqual(t, cryptoIntentUndeliverable(model.GetCryptoPaymentIntent(ctx, held.IntentId)), true)
		_, err = CryptoOperatorCredit(clientSession, held.IntentId, false)
		connect.AssertEqual(t, err != nil && strings.Contains(err.Error(), "unlinked from its network"), true)

		result, err = RunCryptoPaymentUnlink(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Unlinked, 0)
	})
}
