package model

// What refunds, disputes and orphaned crypto payments do to a gift order's
// codes (mmm/GIFTOFPRIVACY.md §5, decision 16; server gift_refund_schema.go).
//
// Decision 16: unredeemed codes can be refunded, and a refund voids them;
// redeemed codes are not refundable. So, for an order:
//
//   - a refund (RefundGiftOrderCodesInTx) voids ⌊the order's cumulative
//     refunded cents / its unit price⌋ codes in all, unredeemed ones only,
//     the highest code first, and never ends a redeemed code's year. Every
//     refund of the order is counted once, by its id, so a refund delivered
//     again, or the next refund of the charge, never voids twice. A refunded
//     total worth more than the unredeemed codes voids them all and says so,
//     for an operator. The order is refunded once no unredeemed code is left;
//     until then it stays paid, with its refunded total recorded;
//   - a dispute opened (DisputeGiftOrderCodesInTx) voids every unredeemed code
//     at once; redeemed years continue;
//   - a dispute lost (LoseGiftOrderDisputeInTx) also ends the redeemed codes'
//     years, and the order is refunded;
//   - a dispute won (WinGiftOrderDisputeInTx) restores exactly the codes that
//     dispute voided and the years it ended, never a refund's, and the order is
//     paid again;
//   - an operator's refund of a crypto order (RefundCryptoGiftOrderInTx) voids
//     every unredeemed code and never ends a year;
//   - a crypto payment that left the chain (endCryptoGiftOrderGrantInTx, from
//     EndCryptoPaymentGrantInTx) voids every unredeemed code and ends the
//     redeemed codes' years; credited again (RestoreCryptoGiftOrderInTx), the
//     order gets exactly those back, unless it was refunded meanwhile.
//
// Each void and each ended year is a gift_code_void row: its cause, its source
// (the refund or the dispute) and its time, the time the year was cut at.
// Every function runs in its caller's transaction, under the order's row lock.
// Ending or restoring years takes the redeemed codes' balances through one
// ownership admission, which needs a fresh ReadCommitted OptNoRetry
// transaction, admits once per transaction, and raises
// errTransferBalanceOwnershipBusy to roll the caller back for a retry. So the
// years of an order end in one clawback over all its codes
// (endGiftOrderCodesInTx), and come back under one admission
// (restoreGiftOrderVoidsInTx).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Why a gift code is void, or its year ended (gift_code_void.cause).
const (
	// a refund: a Stripe refund of the order's charge, or an operator's refund
	// of its crypto invoice. Final: never restored.
	GiftCodeVoidCauseRefund = "refund"
	// a card dispute (source_id: the dispute), restored when it is won
	GiftCodeVoidCauseDispute = "dispute"
	// the order's crypto payment left the chain, restored when it is credited
	// again
	GiftCodeVoidCauseOrphan = "orphan"
)

// The source of an operator's crypto refund's voids (no refund id; the
// invoice is not named, so the order keeps no link to it).
const giftCodeVoidSourceOperator = "operator"

// A refund id of the whole charge rather than one refund (the Stripe handler's
// "charge/<id>" when a charge lists no refund): its amount is the charge's
// cumulative refunded amount, not one more refund.
const giftChargeRefundIdPrefix = "charge/"

// The account message template of a friend's gift email (the controller's
// gift_card_friend), which the order owes while one is unsent.
const giftCardFriendTemplateName = "gift_card_friend"

// An order as its refunds and disputes read it, under its row lock.
type giftRefundOrder struct {
	giftOrderId       server.Id
	status            string
	quantity          int
	unitPriceCents    int64
	delivery          string
	purchaseEventBase *string
}

// One code of an order, by its number n (purchase event "<base>/gift/<n>").
type giftRefundCode struct {
	balanceCodeId   server.Id
	n               int
	purchaseEventId string
	redeemed        bool
	voided          bool
}

// Locks the order's row, nil when there is none.
func lockGiftRefundOrderInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) *giftRefundOrder {
	var order *giftRefundOrder
	result, err := tx.Query(
		ctx,
		`
			SELECT status, quantity, unit_price_cents, delivery, purchase_event_base
			FROM gift_order
			WHERE gift_order_id = $1
			FOR UPDATE
		`,
		giftOrderId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			order = &giftRefundOrder{giftOrderId: giftOrderId}
			server.Raise(result.Scan(
				&order.status,
				&order.quantity,
				&order.unitPriceCents,
				&order.delivery,
				&order.purchaseEventBase,
			))
		}
	})
	return order
}

// The order's codes in code order, by their exact purchase events (the unique
// key); none while it is unpaid.
func giftRefundCodesInTx(tx server.PgTx, ctx context.Context, order *giftRefundOrder) []*giftRefundCode {
	if order.purchaseEventBase == nil || order.quantity < 1 {
		return nil
	}
	purchaseEventIds := make([]string, 0, order.quantity)
	numbers := map[string]int{}
	for n := 0; n < order.quantity; n += 1 {
		purchaseEventId := giftPurchaseEventId(*order.purchaseEventBase, n)
		purchaseEventIds = append(purchaseEventIds, purchaseEventId)
		numbers[purchaseEventId] = n
	}
	codes := []*giftRefundCode{}
	result, err := tx.Query(
		ctx,
		`
			SELECT
				balance_code_id,
				purchase_event_id,
				redeem_balance_id IS NOT NULL,
				cancel_time IS NOT NULL
			FROM transfer_balance_code
			WHERE
				purchase_event_id = ANY($1) AND
				gift_order_id = $2
		`,
		purchaseEventIds,
		order.giftOrderId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			code := &giftRefundCode{}
			server.Raise(result.Scan(&code.balanceCodeId, &code.purchaseEventId, &code.redeemed, &code.voided))
			code.n = numbers[code.purchaseEventId]
			codes = append(codes, code)
		}
	})
	slices.SortFunc(codes, func(a *giftRefundCode, b *giftRefundCode) int {
		return a.n - b.n
	})
	return codes
}

// Voids one unredeemed code and records why. false when it was redeemed or
// void already.
func voidGiftCodeInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	code *giftRefundCode,
	cause string,
	sourceId string,
	now time.Time,
) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE transfer_balance_code
			SET cancel_time = $2
			WHERE
				balance_code_id = $1 AND
				redeem_balance_id IS NULL AND
				cancel_time IS NULL
		`,
		code.balanceCodeId,
		now,
	))
	if tag.RowsAffected() == 0 {
		return false
	}
	recordGiftCodeVoidInTx(tx, ctx, giftOrderId, code.balanceCodeId, cause, sourceId, false, now)
	code.voided = true
	return true
}

func recordGiftCodeVoidInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	balanceCodeId server.Id,
	cause string,
	sourceId string,
	endedYear bool,
	now time.Time,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO gift_code_void (
				balance_code_id,
				gift_order_id,
				cause,
				source_id,
				ended_year,
				void_time
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (balance_code_id) DO UPDATE
			SET
				cause = EXCLUDED.cause,
				source_id = EXCLUDED.source_id,
				ended_year = EXCLUDED.ended_year,
				void_time = EXCLUDED.void_time
		`,
		balanceCodeId,
		giftOrderId,
		cause,
		sourceId,
		endedYear,
		now,
	))
}

// Ends, at now, what the order's codes granted that is still in force: every
// unredeemed code is voided and every redeemed code's year ended (its renewal
// and Pro marker, and its network's running cycle grant when no other
// subscription is left open), in one clawback over all the order's purchase
// events (ClawbackBalanceCodesForPurchaseEventInTx: one ownership admission).
// Each is recorded with the cause and source; a code void before, or a year
// recorded as ended before, is left as it is. Returns the codes it voided, the
// codes whose year it ended, and the networks whose Pro changed.
func endGiftOrderCodesInTx(
	tx server.PgTx,
	ctx context.Context,
	order *giftRefundOrder,
	cause string,
	sourceId string,
	now time.Time,
) (voidedCodeIds []server.Id, endedCodeIds []server.Id, endedNetworkIds []server.Id) {
	codes := giftRefundCodesInTx(tx, ctx, order)
	if len(codes) == 0 {
		return
	}
	recorded := map[server.Id]bool{}
	result, err := tx.Query(
		ctx,
		`SELECT balance_code_id FROM gift_code_void WHERE gift_order_id = $1`,
		order.giftOrderId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceCodeId server.Id
			server.Raise(result.Scan(&balanceCodeId))
			recorded[balanceCodeId] = true
		}
	})
	// what the clawback voids and ends, read first: it reports counts only
	voids := []*giftRefundCode{}
	years := []*giftRefundCode{}
	for _, code := range codes {
		switch {
		case code.redeemed:
			if !recorded[code.balanceCodeId] {
				years = append(years, code)
			}
		case !code.voided:
			voids = append(voids, code)
		}
	}
	if len(voids) == 0 && len(years) == 0 {
		return
	}
	_, _, endedNetworkIds = ClawbackBalanceCodesForPurchaseEventInTx(tx, ctx, *order.purchaseEventBase, now)
	for _, code := range voids {
		recordGiftCodeVoidInTx(tx, ctx, order.giftOrderId, code.balanceCodeId, cause, sourceId, false, now)
		code.voided = true
		voidedCodeIds = append(voidedCodeIds, code.balanceCodeId)
	}
	for _, code := range years {
		recordGiftCodeVoidInTx(tx, ctx, order.giftOrderId, code.balanceCodeId, cause, sourceId, true, now)
		endedCodeIds = append(endedCodeIds, code.balanceCodeId)
	}
	return
}

// The order's codes that are neither redeemed nor void.
func giftUnredeemedCodeCount(codes []*giftRefundCode) (count int) {
	for _, code := range codes {
		if !code.redeemed && !code.voided {
			count += 1
		}
	}
	return
}

// The order's cumulative refunded cents: every refund counted once, by its
// id, and a whole charge's cumulative amount (giftChargeRefundIdPrefix) at
// least as much as the refunds it holds.
func giftOrderRefundedCentsInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) (refundedCents int64) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT GREATEST(
				COALESCE(SUM(amount_cents) FILTER (WHERE NOT starts_with(refund_id, $2)), 0),
				COALESCE(MAX(amount_cents) FILTER (WHERE starts_with(refund_id, $2)), 0)
			)
			FROM gift_order_refund
			WHERE gift_order_id = $1
		`,
		giftOrderId,
		giftChargeRefundIdPrefix,
	).Scan(&refundedCents))
	return
}

// GiftRefundOutcome is what one refund of a gift order's charge did.
type GiftRefundOutcome struct {
	// the order's cumulative refunded amount, every refund counted once
	RefundedCents int64
	// the codes this refund voided, highest first
	VoidedCodeIds []server.Id
	// the refunded total is worth more than the unredeemed codes it could
	// void: every unredeemed code is void, and the rest is for an operator
	Exceeds bool
	// no unredeemed code is left, so the order is refunded
	OrderRefunded bool
	// the order was never fulfilled: it has no codes, and is refunded
	Unfulfilled bool
}

// RefundGiftOrderCodesInTx applies one refund (its id, its amount in cents)
// of a gift order's charge, in the caller's transaction: it records the
// refund once, and voids codes until ⌊the cumulative refunded cents / the
// unit price⌋ codes are void by refunds, unredeemed ones only, the highest
// code first. A redeemed code's year is never ended. nil when there is no
// such order.
func RefundGiftOrderCodesInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	refundId string,
	refundCents int64,
	now time.Time,
) *GiftRefundOutcome {
	order := lockGiftRefundOrderInTx(tx, ctx, giftOrderId)
	if order == nil {
		return nil
	}
	if refundId != "" && 0 < refundCents {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gift_order_refund (refund_id, gift_order_id, amount_cents, refund_time)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (refund_id) DO NOTHING
			`,
			refundId,
			giftOrderId,
			refundCents,
			now,
		))
	}
	outcome := &GiftRefundOutcome{
		RefundedCents: giftOrderRefundedCentsInTx(tx, ctx, giftOrderId),
	}
	codes := giftRefundCodesInTx(tx, ctx, order)
	if len(codes) == 0 {
		// refunded before it was fulfilled (its payment never reached a
		// fulfilment): it must never be fulfilled now
		outcome.Unfulfilled = true
		server.Raise(SetGiftOrderRefundedInTx(tx, ctx, giftOrderId))
		outcome.OrderRefunded = true
		return outcome
	}
	if 0 < order.unitPriceCents {
		var refundVoids int64
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT count(*) FROM gift_code_void WHERE gift_order_id = $1 AND cause = $2`,
			giftOrderId,
			GiftCodeVoidCauseRefund,
		).Scan(&refundVoids))
		need := outcome.RefundedCents/order.unitPriceCents - refundVoids
		for i := len(codes) - 1; 0 <= i && 0 < need; i -= 1 {
			code := codes[i]
			if code.redeemed || code.voided {
				continue
			}
			if voidGiftCodeInTx(tx, ctx, giftOrderId, code, GiftCodeVoidCauseRefund, refundId, now) {
				outcome.VoidedCodeIds = append(outcome.VoidedCodeIds, code.balanceCodeId)
				need -= 1
			}
		}
		outcome.Exceeds = 0 < need
	}
	if giftUnredeemedCodeCount(codes) == 0 {
		server.Raise(SetGiftOrderRefundedInTx(tx, ctx, giftOrderId))
		outcome.OrderRefunded = true
	}
	return outcome
}

// GetGiftOrderRefundedCents is an order's cumulative refunded amount, every
// refund counted once.
func GetGiftOrderRefundedCents(ctx context.Context, giftOrderId server.Id) (refundedCents int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		refundedCents = giftOrderRefundedCentsInTx(tx, ctx, giftOrderId)
	})
	return
}

// DisputeGiftOrderCodesInTx voids every unredeemed code of a gift order whose
// charge is disputed, in the caller's transaction; redeemed years continue,
// and the order's status waits for the dispute's close. Returns the codes it
// voided. A second call for the dispute voids nothing more.
func DisputeGiftOrderCodesInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	disputeId string,
	now time.Time,
) (voidedCodeIds []server.Id) {
	order := lockGiftRefundOrderInTx(tx, ctx, giftOrderId)
	if order == nil {
		return nil
	}
	for _, code := range giftRefundCodesInTx(tx, ctx, order) {
		if code.redeemed || code.voided {
			continue
		}
		if voidGiftCodeInTx(tx, ctx, giftOrderId, code, GiftCodeVoidCauseDispute, disputeId, now) {
			voidedCodeIds = append(voidedCodeIds, code.balanceCodeId)
		}
	}
	return
}

// GiftDisputeLoss is what a lost dispute of a gift order's charge did.
type GiftDisputeLoss struct {
	// unredeemed codes it voided now (an opened dispute already voided them,
	// unless its event was missed)
	VoidedCodeIds []server.Id
	// redeemed codes whose year it ended
	EndedCodeIds []server.Id
	// the networks whose Pro changed, to refresh after commit
	EndedNetworkIds []server.Id
}

// LoseGiftOrderDisputeInTx is a gift order's lost dispute, in the caller's
// transaction (ReadCommitted, OptNoRetry): the funds are gone, so every
// unredeemed code is void, every redeemed code's year ends, and the order is
// refunded.
func LoseGiftOrderDisputeInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	disputeId string,
	now time.Time,
) *GiftDisputeLoss {
	order := lockGiftRefundOrderInTx(tx, ctx, giftOrderId)
	if order == nil {
		return nil
	}
	loss := &GiftDisputeLoss{}
	loss.VoidedCodeIds, loss.EndedCodeIds, loss.EndedNetworkIds = endGiftOrderCodesInTx(
		tx, ctx, order, GiftCodeVoidCauseDispute, disputeId, now,
	)
	server.Raise(SetGiftOrderRefundedInTx(tx, ctx, giftOrderId))
	return loss
}

// A code a dispute won, or a crypto payment credited again, restored.
type GiftRestoredCode struct {
	BalanceCodeId server.Id
	// the code's number, its recipient's index for delivery "them"
	N int
	// a redeemed code whose year was restored, not a voided code
	Year bool
	// when it was voided, or its year cut
	VoidTime time.Time
}

// GiftRestoration is what a won dispute, or a crypto payment credited again,
// restored of a gift order.
type GiftRestoration struct {
	// the order as it is now, with its recipients
	Order *GiftOrder
	Codes []*GiftRestoredCode
	// the networks whose Pro came back, to refresh after commit
	NetworkIds []server.Id
}

// Restores what the cause and source voided and ended of an order, and
// forgets it. A code redeemed or voided again since is left as it is. The
// years come back under one ownership admission of all their balances.
func restoreGiftOrderVoidsInTx(
	tx server.PgTx,
	ctx context.Context,
	order *giftRefundOrder,
	cause string,
	sourceId string,
) *GiftRestoration {
	type voidRow struct {
		balanceCodeId server.Id
		endedYear     bool
		voidTime      time.Time
	}
	rows := []*voidRow{}
	result, err := tx.Query(
		ctx,
		`
			SELECT balance_code_id, ended_year, void_time
			FROM gift_code_void
			WHERE gift_order_id = $1 AND cause = $2 AND source_id = $3
			ORDER BY balance_code_id
		`,
		order.giftOrderId,
		cause,
		sourceId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			row := &voidRow{}
			server.Raise(result.Scan(&row.balanceCodeId, &row.endedYear, &row.voidTime))
			rows = append(rows, row)
		}
	})
	restoration := &GiftRestoration{}
	if len(rows) == 0 {
		return restoration
	}
	numbers := map[server.Id]int{}
	for _, code := range giftRefundCodesInTx(tx, ctx, order) {
		numbers[code.balanceCodeId] = code.n
	}

	// the years first: each one to put back is read, then all their balances
	// are admitted at once, then each is put back
	years := map[server.Id]*giftYearRestore{}
	balanceIds := []server.Id{}
	for _, row := range rows {
		if !row.endedYear {
			continue
		}
		if year := planGiftYearRestoreInTx(tx, ctx, row.balanceCodeId, row.voidTime); year != nil {
			years[row.balanceCodeId] = year
			balanceIds = append(balanceIds, year.markerId)
			balanceIds = append(balanceIds, year.cycleBalanceIds...)
		}
	}
	if 0 < len(balanceIds) {
		slices.SortFunc(balanceIds, func(a server.Id, b server.Id) int {
			return a.Cmp(b)
		})
		admitted, ownershipErr := tryTransferBalanceOwnershipInTx(ctx, tx, slices.Compact(balanceIds))
		server.Raise(ownershipErr)
		if !admitted {
			server.Raise(errTransferBalanceOwnershipBusy)
		}
	}

	for _, row := range rows {
		restored := false
		if row.endedYear {
			if year := years[row.balanceCodeId]; year != nil {
				applyGiftYearRestoreInTx(tx, ctx, year)
				restoration.NetworkIds = append(restoration.NetworkIds, year.networkId)
				restored = true
			}
		} else {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE transfer_balance_code
					SET cancel_time = NULL
					WHERE
						balance_code_id = $1 AND
						redeem_balance_id IS NULL AND
						cancel_time IS NOT NULL
				`,
				row.balanceCodeId,
			))
			restored = tag.RowsAffected() == 1
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM gift_code_void WHERE balance_code_id = $1`,
			row.balanceCodeId,
		))
		if restored {
			restoration.Codes = append(restoration.Codes, &GiftRestoredCode{
				BalanceCodeId: row.balanceCodeId,
				N:             numbers[row.balanceCodeId],
				Year:          row.endedYear,
				VoidTime:      row.voidTime,
			})
		}
	}
	slices.SortFunc(restoration.Codes, func(a *GiftRestoredCode, b *GiftRestoredCode) int {
		return a.N - b.N
	})
	slices.SortFunc(restoration.NetworkIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	restoration.NetworkIds = slices.Compact(restoration.NetworkIds)
	return restoration
}

// A redeemed code's year to put back: cut at cutTime, from its renewal
// (market gift, the code's id) and Pro marker, with the cycle grants of its
// network the cut ended.
type giftYearRestore struct {
	balanceCodeId   server.Id
	networkId       server.Id
	markerId        server.Id
	startTime       time.Time
	endTime         time.Time
	cutTime         time.Time
	cycleBalanceIds []server.Id
}

// Reads a redeemed code's year that was cut at cutTime, to put it back (as
// RestoreDisputedStripeEntitlementInTx restores a disputed card period). nil
// when nothing was cut there: the year ended on its own, its renewal no longer
// ends at the cut, or the network is gone.
func planGiftYearRestoreInTx(tx server.PgTx, ctx context.Context, balanceCodeId server.Id, cutTime time.Time) *giftYearRestore {
	var networkId *server.Id
	var markerId *server.Id
	var startTime time.Time
	var endTime time.Time
	found := false
	result, err := tx.Query(
		ctx,
		`
			SELECT network_id, redeem_balance_id, start_time, end_time
			FROM transfer_balance_code
			WHERE balance_code_id = $1 AND redeem_balance_id IS NOT NULL
		`,
		balanceCodeId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&networkId, &markerId, &startTime, &endTime))
			found = true
		}
	})
	if !found || networkId == nil || markerId == nil || !cutTime.Before(endTime) {
		return nil
	}
	if err := LockPaymentNetworkInTx(tx, ctx, *networkId); err != nil {
		if errors.Is(err, ErrPaymentNetworkNotFound) {
			return nil
		}
		server.Raise(err)
	}

	var renewalEnd time.Time
	renewalFound := false
	result, err = tx.Query(
		ctx,
		`
			SELECT end_time FROM subscription_renewal
			WHERE
				network_id = $1 AND
				subscription_type = $2 AND
				market = $3 AND
				transaction_id = $4 AND
				start_time = $5
			FOR UPDATE
		`,
		*networkId,
		SubscriptionTypeSupporter,
		SubscriptionMarketGift,
		balanceCodeId.String(),
		startTime,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&renewalEnd))
			renewalFound = true
		}
	})
	if !renewalFound || !renewalEnd.Equal(cutTime) {
		return nil
	}

	year := &giftYearRestore{
		balanceCodeId: balanceCodeId,
		networkId:     *networkId,
		markerId:      *markerId,
		startTime:     startTime,
		endTime:       endTime,
		cutTime:       cutTime,
	}
	// the network's cycle grant the cut ended with the year
	result, err = tx.Query(
		ctx,
		`
			SELECT cycle.balance_id
			FROM pro_grant_cycle AS cycle
			INNER JOIN transfer_balance AS balance ON balance.balance_id = cycle.balance_id
			WHERE
				cycle.network_id = $1 AND
				cycle.ended_time IS NOT NULL AND
				balance.end_time = $2
		`,
		*networkId,
		cutTime,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			server.Raise(result.Scan(&balanceId))
			year.cycleBalanceIds = append(year.cycleBalanceIds, balanceId)
		}
	})
	return year
}

// Puts back a planned year, its balances admitted: the gift renewal and Pro
// marker to the year's end, and the cycle grants the cut ended to their
// cycle's end with the Pro grant's grace. A second year of the same network
// and cut finds its cycle grants restored already.
func applyGiftYearRestoreInTx(tx server.PgTx, ctx context.Context, year *giftYearRestore) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE subscription_renewal SET end_time = $1
			WHERE
				network_id = $2 AND
				subscription_type = $3 AND
				market = $4 AND
				transaction_id = $5 AND
				start_time = $6 AND
				end_time = $7
		`,
		year.endTime,
		year.networkId,
		SubscriptionTypeSupporter,
		SubscriptionMarketGift,
		year.balanceCodeId.String(),
		year.startTime,
		year.cutTime,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE transfer_balance SET end_time = $1 WHERE balance_id = $2 AND end_time = $3`,
		year.endTime,
		year.markerId,
		year.cutTime,
	))
	if len(year.cycleBalanceIds) == 0 {
		return
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			WITH restored AS (
				UPDATE pro_grant_cycle AS cycle SET ended_time = NULL
				WHERE
					cycle.network_id = $1 AND
					cycle.balance_id = ANY($2::uuid[]) AND
					cycle.ended_time IS NOT NULL
				RETURNING cycle.balance_id, cycle.cycle_end_time
			)
			UPDATE transfer_balance AS balance SET end_time = restored.cycle_end_time + $4::interval
			FROM restored
			WHERE
				balance.balance_id = restored.balance_id AND
				balance.end_time = $3
		`,
		year.networkId,
		year.cycleBalanceIds,
		year.cutTime,
		fmt.Sprintf("%d seconds", int64(ProGrantGrace/time.Second)),
	))
}

// Reads into the restoration what owing a friend's email again needs of the
// order: its delivery, its send date and zone, when it was made and paid,
// whether its cards were rendered, and its recipients in code order.
func giftRestorationOrderInTx(tx server.PgTx, ctx context.Context, restoration *GiftRestoration, giftOrderId server.Id) {
	order := &GiftOrder{GiftOrderId: giftOrderId, Recipients: []string{}}
	found := false
	result, err := tx.Query(
		ctx,
		`
			SELECT
				status,
				delivery,
				send_on,
				send_timezone,
				create_time,
				paid_time,
				EXISTS (
					SELECT 1 FROM gift_order_pdf
					WHERE gift_order_pdf.gift_order_id = gift_order.gift_order_id AND part = $2
				)
			FROM gift_order
			WHERE gift_order_id = $1
		`,
		giftOrderId,
		GiftOrderPdfPartAll,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(
				&order.Status,
				&order.Delivery,
				&order.SendOn,
				&order.SendTimezone,
				&order.CreateTime,
				&order.PaidTime,
				&order.PdfReady,
			))
			found = true
		}
	})
	if !found {
		return
	}
	result, err = tx.Query(
		ctx,
		`
			SELECT email
			FROM gift_order_recipient
			WHERE gift_order_id = $1
			ORDER BY recipient_index
		`,
		giftOrderId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var email string
			server.Raise(result.Scan(&email))
			order.Recipients = append(order.Recipients, email)
		}
	})
	restoration.Order = order
}

// WinGiftOrderDisputeInTx is a gift order's won dispute, in the caller's
// transaction (ReadCommitted, OptNoRetry): exactly the codes the dispute
// voided and the years it ended come back, never a refund's voids, and the
// order is paid again. nil when there is no such order.
func WinGiftOrderDisputeInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	disputeId string,
) *GiftRestoration {
	order := lockGiftRefundOrderInTx(tx, ctx, giftOrderId)
	if order == nil {
		return nil
	}
	restoration := restoreGiftOrderVoidsInTx(tx, ctx, order, GiftCodeVoidCauseDispute, disputeId)
	if 0 < len(restoration.Codes) && order.status == GiftOrderStatusRefunded {
		setGiftOrderPaidAgainInTx(tx, ctx, giftOrderId)
	}
	giftRestorationOrderInTx(tx, ctx, restoration, giftOrderId)
	return restoration
}

// The order is paid again: a won dispute gave back what its loss refunded.
func setGiftOrderPaidAgainInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gift_order
			SET status = $2, refunded_time = NULL
			WHERE gift_order_id = $1 AND status = $3
		`,
		giftOrderId,
		GiftOrderStatusPaid,
		GiftOrderStatusRefunded,
	))
}

// GiftCryptoRefund is what an operator's refund of a gift order's crypto
// invoice did: the codes it voided are the ones to pay back.
type GiftCryptoRefund struct {
	Quantity int
	// the unredeemed codes it voided, the ones refunded
	Refunded int
	// the redeemed codes, which are not refundable
	Redeemed int
	// void before, by another cause
	AlreadyVoid int
}

// RefundCryptoGiftOrderInTx is an operator's refund of a gift order's crypto
// invoice, in the caller's transaction: it voids every unredeemed code and
// never ends a redeemed code's year, and the order is refunded when it voided
// any. The caller pays back the refunded codes' share of what was received.
// nil when there is no such order.
func RefundCryptoGiftOrderInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	now time.Time,
) *GiftCryptoRefund {
	order := lockGiftRefundOrderInTx(tx, ctx, giftOrderId)
	if order == nil {
		return nil
	}
	refund := &GiftCryptoRefund{Quantity: order.quantity}
	for _, code := range giftRefundCodesInTx(tx, ctx, order) {
		switch {
		case code.redeemed:
			refund.Redeemed += 1
		case code.voided:
			refund.AlreadyVoid += 1
		default:
			if voidGiftCodeInTx(tx, ctx, giftOrderId, code, GiftCodeVoidCauseRefund, giftCodeVoidSourceOperator, now) {
				refund.Refunded += 1
			}
		}
	}
	if 0 < refund.Refunded {
		server.Raise(SetGiftOrderRefundedInTx(tx, ctx, giftOrderId))
	}
	return refund
}

// The gift order an invoice paid, nil for any other invoice.
func cryptoGiftOrderIdInTx(tx server.PgTx, ctx context.Context, intentId server.Id) (giftOrderId *server.Id) {
	result, err := tx.Query(
		ctx,
		`SELECT gift_order_id FROM gift_order WHERE crypto_intent_id = $1`,
		intentId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&giftOrderId))
		}
	})
	return
}

// endCryptoGiftOrderGrantInTx ends, at now, what a gift order's invoice
// granted when its payment left the chain (EndCryptoPaymentGrantInTx, for the
// reconcile's orphan leg, in its ReadCommitted OptNoRetry transaction): every
// unredeemed code is voided and every redeemed code's year ended, each
// recorded, so a credit again restores exactly them
// (RestoreCryptoGiftOrderInTx). The order stays paid.
func endCryptoGiftOrderGrantInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	now time.Time,
) (endedNetworkIds []server.Id, ended bool) {
	order := lockGiftRefundOrderInTx(tx, ctx, giftOrderId)
	if order == nil {
		return nil, false
	}
	voidedCodeIds, endedCodeIds, endedNetworkIds := endGiftOrderCodesInTx(tx, ctx, order, GiftCodeVoidCauseOrphan, "", now)
	return endedNetworkIds, 0 < len(voidedCodeIds) || 0 < len(endedCodeIds)
}

// RestoreCryptoGiftOrderInTx restores, in the credit's transaction
// (ReadCommitted, OptNoRetry), what a gift order's invoice lost when its
// payment left the chain, now that the payment is credited again: the
// unredeemed codes the orphan leg voided (the codes on the order's cards) and
// the redeemed codes' years it ended. A refunded order's codes are never
// restored; a refund's voids are final. nil when the order is not paid.
func RestoreCryptoGiftOrderInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
) *GiftRestoration {
	order := lockGiftRefundOrderInTx(tx, ctx, giftOrderId)
	if order == nil || order.status != GiftOrderStatusPaid {
		return nil
	}
	restoration := restoreGiftOrderVoidsInTx(tx, ctx, order, GiftCodeVoidCauseOrphan, "")
	giftRestorationOrderInTx(tx, ctx, restoration, giftOrderId)
	return restoration
}

// ClearUnlinkedGiftOrderBuyerEmail blanks the buyer's email of a gift order
// whose crypto invoice the 30-day retention unlink has passed (rail crypto, no
// invoice id), once no friend email of the order is owed any more: none of its
// gift_card_friend messages is unsent and not given up, and for delivery
// "them" its cards are rendered (until then its friend emails are still to be
// owed). The unlink blanks it itself when nothing is owed then; while a
// friend's email is owed the address stays, as that email's reply-to, until
// this runs. The delivery calls it after an order's friend email is sent or
// given up, and the daily retention may call it for any order; it does
// nothing for any other order, and is idempotent. cleared is true when it
// blanked the address.
func ClearUnlinkedGiftOrderBuyerEmail(ctx context.Context, giftOrderId server.Id) (cleared bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		cleared = clearUnlinkedGiftOrderBuyerEmailInTx(tx, ctx, giftOrderId)
	})
	return
}

func clearUnlinkedGiftOrderBuyerEmailInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gift_order
			SET buyer_email = ''
			WHERE
				gift_order_id = $1 AND
				rail = $2 AND
				crypto_intent_id IS NULL AND
				buyer_email <> '' AND
				NOT (
					delivery = $3 AND
					status = $4 AND
					NOT EXISTS (
						SELECT 1 FROM gift_order_pdf
						WHERE gift_order_pdf.gift_order_id = gift_order.gift_order_id AND part = $5
					)
				) AND
				NOT EXISTS (
					SELECT 1 FROM account_message_outbox message
					WHERE
						message.deliver_time IS NOT NULL AND
						message.sent_time IS NULL AND
						message.abandon_time IS NULL AND
						message.template_name = $6 AND
						message.template_json ->> 'gift_order_id' = $1::text
				)
		`,
		giftOrderId,
		GiftRailCrypto,
		GiftDeliveryThem,
		GiftOrderStatusPaid,
		GiftOrderPdfPartAll,
		giftCardFriendTemplateName,
	))
	return tag.RowsAffected() == 1
}

// The outbox state of one message (the sent and given-up times).
type AccountMessageState struct {
	DeliverTime *time.Time
	SentTime    *time.Time
	AbandonTime *time.Time
}

// GetGiftFriendEmailStatesInTx is the outbox state of the latest friend's
// email (gift_card_friend) of each recipient of the order, under whichever key
// owed it (the first, or one owed again), by recipient index, in the caller's
// transaction. A recipient whose emails the outbox removed has none.
func GetGiftFriendEmailStatesInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) map[int]*AccountMessageState {
	states := map[int]*AccountMessageState{}
	result, err := tx.Query(
		ctx,
		`
			SELECT DISTINCT ON (recipient_index)
				recipient_index,
				deliver_time,
				sent_time,
				abandon_time
			FROM (
				SELECT
					(template_json ->> 'recipient_index')::integer AS recipient_index,
					deliver_time,
					sent_time,
					abandon_time,
					create_time,
					message_id
				FROM account_message_outbox
				WHERE
					template_name = $1 AND
					template_json ->> 'gift_order_id' = $2
			) AS message
			ORDER BY recipient_index, create_time DESC, message_id DESC
		`,
		giftCardFriendTemplateName,
		giftOrderId.String(),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var recipientIndex int
			state := &AccountMessageState{}
			server.Raise(result.Scan(&recipientIndex, &state.DeliverTime, &state.SentTime, &state.AbandonTime))
			states[recipientIndex] = state
		}
	})
	return states
}
