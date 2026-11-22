package model

// The 30-day unlink of a gift order from the payment that paid it, or was
// made for it (mmm/GIFTOFPRIVACY.md §9.1, call 5): a coin's invoice
// (UnlinkCryptoPaymentIntents, through unlinkCryptoGiftOrderInTx) and a Pay
// with Gamma intent (UnlinkGammaGiftOrders, which the gift retention runs
// daily). The order keeps its codes, so its cards still deliver and its codes
// still redeem; it forgets which payment it was, and the facts that would
// tie it to one. Neither unlinks an order whose payment arrived and may still
// be credited: a coin's invoice that received a payment it was not credited
// for stays linked until the payment is credited, the invoice is voided or
// refunded, or nothing holds it (cryptoGiftInvoiceHeldSql), as a Gamma
// intent with a charge that can still land does.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A payment a gift order is linked to until the 30-day unlink.
type giftOrderPaymentLink struct {
	// the gift_order column that holds the payment's id (a source literal)
	column string
	// the purchase event base of the codes the payment made
	purchaseEventBase func(intentId server.Id) string
	// where an unlinked order's codes move: this, then a fresh random id
	unlinkedPrefix string
}

var (
	// a coin's invoice: codes crypto/<intent id>/gift/<n>
	giftOrderCryptoLink = &giftOrderPaymentLink{
		column:            "crypto_intent_id",
		purchaseEventBase: CryptoPaymentPurchaseEventId,
		unlinkedPrefix:    "crypto/unlinked/",
	}
	// a Pay with Gamma intent: codes gamma/<intent id>/gift/<n>
	giftOrderGammaLink = &giftOrderPaymentLink{
		column:            "gamma_intent_id",
		purchaseEventBase: GammaPaymentPurchaseEventId,
		unlinkedPrefix:    "gamma/unlinked/",
	}
)

// unlinkGiftOrderPaymentInTx is the 30-day unlink of the gift order the
// payment paid for, or was made for, in the caller's transaction: the order
// forgets the payment's id, and its codes' purchase events, "<payment
// base>/gift/<n>", become "<base>/gift/<n>" under a fresh random base (the
// link's unlinked prefix and a new id), which the order takes as its own
// purchase event base, so it still finds its codes (GetGiftOrderCodes); they
// lose their purchase record and email. The codes are matched by their exact
// purchase event ids, the unique key. An order never paid by it only forgets
// the id. Then the order's facts go (unlinkGiftOrderFactsInTx). Nothing for a
// payment no gift order names.
func unlinkGiftOrderPaymentInTx(tx server.PgTx, ctx context.Context, link *giftOrderPaymentLink, intentId server.Id) {
	var giftOrderId server.Id
	var quantity int
	var purchaseEventBase *string
	found := false
	result, err := tx.Query(
		ctx,
		fmt.Sprintf(
			`
				SELECT gift_order_id, quantity, purchase_event_base
				FROM gift_order
				WHERE %s = $1
				FOR UPDATE
			`,
			link.column,
		),
		intentId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&giftOrderId, &quantity, &purchaseEventBase))
			found = true
		}
	})
	if !found {
		return
	}

	paymentBase := link.purchaseEventBase(intentId)
	if purchaseEventBase == nil || *purchaseEventBase != paymentBase {
		// never paid: no code names the payment
		server.RaisePgResult(tx.Exec(
			ctx,
			fmt.Sprintf(`UPDATE gift_order SET %s = NULL WHERE gift_order_id = $1`, link.column),
			giftOrderId,
		))
		unlinkGiftOrderFactsInTx(tx, ctx, giftOrderId)
		return
	}
	unlinkedBase := link.unlinkedPrefix + server.NewId().String()
	fromIds := make([]string, 0, quantity)
	toIds := make([]string, 0, quantity)
	for n := 0; n < quantity; n += 1 {
		fromIds = append(fromIds, giftPurchaseEventId(paymentBase, n))
		toIds = append(toIds, giftPurchaseEventId(unlinkedBase, n))
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE transfer_balance_code
			SET
				purchase_event_id = unlinked.to_id,
				purchase_record = '',
				purchase_email = ''
			FROM unnest($1::text[], $2::text[]) AS unlinked(from_id, to_id)
			WHERE
				transfer_balance_code.purchase_event_id = unlinked.from_id AND
				transfer_balance_code.gift_order_id = $3
		`,
		fromIds,
		toIds,
		giftOrderId,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		fmt.Sprintf(
			`
				UPDATE gift_order
				SET %s = NULL, purchase_event_base = $2
				WHERE gift_order_id = $1
			`,
			link.column,
		),
		giftOrderId,
		unlinkedBase,
	))
	unlinkGiftOrderFactsInTx(tx, ctx, giftOrderId)
}

// What else of a gift order could tie it to its unlinked payment: its times
// become their day, as an unlinked invoice's do, and so do its codes' (made at
// the credit, voided at an operator's refund or the orphan leg, each the time
// of an event or a transaction that names the payment): create_time and
// cancel_time, and gift_code_void's void_time. A void code stays void
// (cancel_time stays set), and nothing reads those times but whether one is
// set: a code's redeem window is its start_time and end_time, which its
// fulfilment sets on day boundaries already. Its referrer goes (its codes keep
// theirs, which the referral credit at redemption reads); and its buyer's
// email is blanked unless a friend's email is still owed, whose reply-to it is
// (ClearUnlinkedGiftOrderBuyerEmail blanks it once none is).
func unlinkGiftOrderFactsInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gift_order
			SET
				create_time = date_trunc('day', create_time),
				paid_time = date_trunc('day', paid_time),
				refunded_time = date_trunc('day', refunded_time),
				referrer_network_id = NULL
			WHERE gift_order_id = $1
		`,
		giftOrderId,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE transfer_balance_code
			SET
				create_time = date_trunc('day', create_time),
				cancel_time = date_trunc('day', cancel_time)
			WHERE gift_order_id = $1
		`,
		giftOrderId,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gift_code_void
			SET void_time = date_trunc('day', void_time)
			WHERE gift_order_id = $1
		`,
		giftOrderId,
	))
	clearUnlinkedGiftOrderBuyerEmailInTx(tx, ctx, giftOrderId)
}

// GiftGammaUnlinkAfter is how long a gift order paid with Gamma stays linked
// to its intent: 30 days, the crypto rail's unlink_after (crypto_pay.yml,
// 720h by default).
const GiftGammaUnlinkAfter = 720 * time.Hour

// UnlinkGammaGiftOrders is the 30-day unlink of gift orders from their Pay
// with Gamma intents, as a coin's order is unlinked from its invoice
// (UnlinkCryptoPaymentIntents). An order forgets its intent once the intent:
//   - was credited before cutoff;
//   - or, never credited and made before cutoff, expired by now with no charge
//     that is pending (a pending charge of an open intent may still go into a
//     spend) or spent (a spent charge is credited from the order this finds);
//   - or, never credited and made before cutoff, was voided before cutoff (by
//     a newer gift purchase, a cancel, the account's deletion, or an
//     operator's disposition of a spend whose credit kept failing) with no
//     charge in a spend batch that can still land. A voided intent is never
//     charged again or credited: its pending charge in no batch never joins
//     one (the account's deletion leaves such a charge pending), a skipped or
//     failed one is never re-armed, and a spent one is the operator's,
//     disposed of when the intent was voided. Only a charge already in a live
//     batch, whose spend may still land, keeps the order linked until the
//     batch lands or expires.
//
// For each, in one transaction
// (unlinkGiftOrderPaymentInTx): its codes' purchase events
// "gamma/<intent id>/gift/<n>" move to a fresh base, "gamma/unlinked/<id>",
// the order's own, and lose their purchase record and email; the order loses
// the intent's id, its exact times (its day stays) and its referrer (its codes
// keep theirs), and its buyer's email once no friend's email is owed. Its codes
// still redeem and its cards still deliver.
//
// The intent keeps what Pay with Gamma keeps of every payment: the network
// that paid, its deposit hotkey, its public reference and its price, which the
// chain shows anyway. It never named the order, and a gift intent's credit
// records no grant, so nothing of it names a code. Someone with database
// access could still match a price and a day, as with an unlinked invoice. One
// batch per call, oldest order first; the caller repeats while a batch comes
// back full.
func UnlinkGammaGiftOrders(ctx context.Context, cutoff time.Time, limit int, now time.Time) (unlinked int) {
	server.Tx(ctx, func(tx server.PgTx) {
		unlinked = 0
		intentIds := []server.Id{}
		result, err := tx.Query(
			ctx,
			`
				SELECT gift_order.gamma_intent_id
				FROM gift_order
				INNER JOIN gamma_payment_intent intent ON intent.intent_id = gift_order.gamma_intent_id
				WHERE
					gift_order.gamma_intent_id IS NOT NULL AND
					(
						intent.credited_at < $1 OR
						(
							intent.credited_at IS NULL AND
							intent.create_time < $1 AND
							(
								(
									intent.voided_at IS NULL AND
									intent.expires_at <= $2 AND
									NOT EXISTS (
										SELECT 1
										FROM gamma_charge charge
										WHERE
											charge.intent_id = intent.intent_id AND
											charge.state IN ($3, $4)
									)
								) OR
								(
									intent.voided_at < $1 AND
									NOT EXISTS (
										SELECT 1
										FROM gamma_charge charge
										WHERE
											charge.intent_id = intent.intent_id AND
											charge.state = $3 AND
											charge.batch_id IS NOT NULL
									)
								)
							)
						)
					)
				ORDER BY gift_order.create_time, gift_order.gift_order_id
				LIMIT $5
				FOR UPDATE OF gift_order SKIP LOCKED
			`,
			cutoff,
			now,
			GammaChargePending,
			GammaChargeSpent,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var intentId server.Id
				server.Raise(result.Scan(&intentId))
				intentIds = append(intentIds, intentId)
			}
		})
		for _, intentId := range intentIds {
			unlinkGiftOrderPaymentInTx(tx, ctx, giftOrderGammaLink, intentId)
			unlinked += 1
		}
	}, server.TxReadCommitted)
	return
}
