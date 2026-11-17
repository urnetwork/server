package model

// Gift orders paid with Gamma: one open gift purchase per account
// (controller/gift_gamma_controller.go).
//
// Every gift order paid with Gamma has a Gamma purchase of its own, open for
// the intent's lifetime (a day), and every one draws on the account's one
// deposit hotkey. So a buyer who starts a gift with Gamma and then changes it
// (another order) or leaves for a card, and mints later, must never be charged
// for the purchase left behind, or for two orders. A new gift purchase voids
// the account's other open gift purchases in the transaction that records it
// (VoidOpenGammaGiftIntentsInTx), and POST /pay/gift/gamma-cancel voids an
// order's (CancelGammaGiftIntent). Neither touches a plan or data purchase,
// and neither voids a purchase whose spend is in flight: a pending charge in
// a spend batch (signed or submitted, which can still land) or a spent charge
// not yet credited, since a spend that lands for a voided intent is never
// credited. The caller refuses instead (payment_in_progress).
//
// A voided intent is never charged. The biller makes a charge only for an
// open intent (CreateGammaCharge, which share-locks the intent, so it is
// ordered with a void), puts only open intents' pending charges in no batch
// into a batch (ListUnbatchedGammaCharges, CreateGammaSpendBatch), and re-arms
// only open intents' skipped charges (ListSkippedGammaCharges,
// ReArmGammaCharge). The void locks the intents and their charges and moves
// every charge it can still reach to voided (GammaChargeVoided): a spend
// batch joins a charge by updating its row, so it is ordered with the void
// too, and a voided charge never joins a batch or is re-armed whatever an
// older snapshot read of its intent.

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// GammaGiftItem is the Pay with Gamma intent item of a gift order's codes, the
// coin rails' gift item.
const GammaGiftItem = "gift_pro_year"

// HasGammaGiftChargeInFlight reports whether a gift purchase of the network
// has a spend in flight: a pending charge in a spend batch, or a spent charge
// whose intent is not yet credited. A new gift purchase is refused meanwhile;
// VoidOpenGammaGiftIntentsInTx decides again under its locks.
func HasGammaGiftChargeInFlight(ctx context.Context, networkId server.Id) (inFlight bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM gamma_payment_intent
					INNER JOIN gamma_charge ON gamma_charge.intent_id = gamma_payment_intent.intent_id
					WHERE gamma_payment_intent.network_id = $1
					  AND gamma_payment_intent.item = $2
					  AND gamma_payment_intent.credited_at IS NULL
					  AND gamma_payment_intent.voided_at IS NULL
					  AND `+gammaChargeInFlightSql+`
				)
			`,
			networkId,
			GammaGiftItem,
		).Scan(&inFlight))
	})
	return
}

// VoidOpenGammaGiftIntentsInTx voids the network's open gift purchases (item
// gift_pro_year, neither credited nor voided), in the caller's ReadCommitted
// transaction, which records the network's new gift purchase: it locks them
// and their charges first. When any has a spend in flight, it writes nothing
// and reports inFlight: none may be voided, and the new purchase is refused.
// Plan and data purchases are never touched. The voided intents are returned.
//
// The network's gift purchases are serialized by an advisory lock held to
// the end of the caller's transaction: of two made at once, the second reads
// the first's intent once it is committed, and voids it.
func VoidOpenGammaGiftIntentsInTx(tx server.PgTx, ctx context.Context, networkId server.Id, now time.Time) (voided []server.Id, inFlight bool) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`/* gamma-gift-purchase-lock */ SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"gamma-gift-purchase:"+networkId.String(),
	))
	// a statement after the lock: its snapshot is taken once the lock is held
	intentIds := []server.Id{}
	result, err := tx.Query(
		ctx,
		`
			SELECT intent_id
			FROM gamma_payment_intent
			WHERE network_id = $1
			  AND item = $2
			  AND credited_at IS NULL
			  AND voided_at IS NULL
			ORDER BY intent_id
			FOR UPDATE
		`,
		networkId,
		GammaGiftItem,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var intentId server.Id
			server.Raise(result.Scan(&intentId))
			intentIds = append(intentIds, intentId)
		}
	})
	if len(intentIds) == 0 {
		return nil, false
	}
	if voidGammaGiftIntentsInTx(tx, ctx, intentIds, now) {
		return nil, true
	}
	return intentIds, false
}

// What cancelling an order's gift purchase did (CancelGammaGiftIntent).
const (
	// voided now: it can never be charged
	GammaGiftCancelVoided = "voided"
	// voided before (by a newer gift purchase, or a cancel)
	GammaGiftCancelClosed = "closed"
	// already credited: the order is paid
	GammaGiftCancelCredited = "credited"
	// its spend is in flight: it cannot be voided now
	GammaGiftCancelInFlight = "in_flight"
	// no such intent, or not a gift purchase
	GammaGiftCancelNotFound = "not_found"
)

// CancelGammaGiftIntent voids a gift order's Gamma purchase unless its spend is
// in flight, in its own ReadCommitted transaction, under the intent's and its
// charges' locks, and says what it did (GammaGiftCancel*). Only a gift
// purchase is ever voided here.
func CancelGammaGiftIntent(ctx context.Context, intentId server.Id, now time.Time) (outcome string) {
	server.Tx(ctx, func(tx server.PgTx) {
		outcome = GammaGiftCancelNotFound
		var found, credited, voided bool
		result, err := tx.Query(
			ctx,
			`
				SELECT credited_at IS NOT NULL, voided_at IS NOT NULL
				FROM gamma_payment_intent
				WHERE intent_id = $1 AND item = $2
				FOR UPDATE
			`,
			intentId,
			GammaGiftItem,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(&credited, &voided))
			}
		})
		switch {
		case !found:
			return
		case credited:
			outcome = GammaGiftCancelCredited
			return
		case voided:
			outcome = GammaGiftCancelClosed
			return
		}
		if voidGammaGiftIntentsInTx(tx, ctx, []server.Id{intentId}, now) {
			outcome = GammaGiftCancelInFlight
			return
		}
		outcome = GammaGiftCancelVoided
	}, server.TxReadCommitted)
	return
}

// Voids open gift intents the caller has locked (FOR UPDATE), in its
// ReadCommitted transaction: their charges are locked, each read as committed
// now. When any has a spend in flight, nothing is written and true is
// returned. Otherwise every charge that can still be charged is voided (a
// pending charge in no batch, or a skipped or failed one, which keeps its old
// batch id), and then the intents.
func voidGammaGiftIntentsInTx(tx server.PgTx, ctx context.Context, intentIds []server.Id, now time.Time) (inFlight bool) {
	result, err := tx.Query(
		ctx,
		`
			SELECT `+gammaChargeInFlightSql+`
			FROM gamma_charge
			WHERE intent_id = ANY($1)
			ORDER BY reference
			FOR UPDATE
		`,
		intentIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var chargeInFlight bool
			server.Raise(result.Scan(&chargeInFlight))
			inFlight = inFlight || chargeInFlight
		}
	})
	if inFlight {
		return true
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gamma_charge
			SET state = 'voided', resolved_at = COALESCE(resolved_at, $2)
			WHERE intent_id = ANY($1)
			  AND (
				(state = 'pending' AND batch_id IS NULL)
				OR state IN ('skipped', 'failed')
			  )
		`,
		intentIds,
		now,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gamma_payment_intent
			SET voided_at = $2
			WHERE intent_id = ANY($1) AND credited_at IS NULL AND voided_at IS NULL
		`,
		intentIds,
		now,
	))
	return false
}
