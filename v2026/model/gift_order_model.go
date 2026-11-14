package model

// Gift orders and their Pro gift codes (mmm/GIFTOFPRIVACY.md §5, §10.3,
// §10.6; server gift_schema.go).
//
// A gift order is one purchase of 1 to gift.max_quantity codes, each a year of
// URnetwork Pro from its redemption. The order is created before payment
// (pending), keyed to its Stripe Checkout session or crypto invoice, and
// fulfilled once on payment (FulfillGiftOrderInTx): its codes are created, in
// order, as transfer_balance_code rows of kind pro_gift, keyed by the purchase
// event "<base>/gift/<n>" (base: the Stripe session id or "crypto/<intent
// id>"), so the existing refund clawback, which matches a session's or an
// invoice's id as a prefix, voids an order's unredeemed codes and ends its
// redeemed ones' grants. Codes are numbered from 0; for delivery "them", code
// n is recipient n's.
//
// A code is redeemable until its redeem-by, gift.redeemable_for after the start
// of the day it was bought (the code's end_time until it is redeemed, and the
// order's redeem_by always). Redemption is in RedeemBalanceCodeInTx
// (gift_redeem_model.go).

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/urnetwork/server/v2026"
)

const (
	GiftOrderStatusPending, GiftOrderStatusPaid, GiftOrderStatusRefunded = "pending", "paid", "refunded"
	GiftDeliveryMe, GiftDeliveryThem                                     = "me", "them"
	GiftRailStripe, GiftRailCrypto                                       = "stripe", "crypto"
	BalanceCodeKindData, BalanceCodeKindProGift                          = "data", "pro_gift"
	// the renewal market of a redeemed gift. It carries its code's net
	// revenue, so the year's traffic is paid (gift_redeem_model.go), and stays
	// out of SubscriptionMarkets like manual: the recipient did not pay.
	SubscriptionMarketGift = "gift"
)

// The status of a code (GiftCodeStatus.Status).
const (
	GiftCodeStatusOk       = "ok"
	GiftCodeStatusRedeemed = "redeemed"
	GiftCodeStatusExpired  = "expired"
	GiftCodeStatusVoided   = "voided"
	GiftCodeStatusNotFound = "not_found"
)

// The PDF parts of an order (PutGiftOrderPdf): every card in one PDF, and for
// delivery "them" one card per recipient (GiftOrderPdfRecipientPart).
const GiftOrderPdfPartAll = "all"

// GiftOrderPdfRecipientPart is the part of recipient index's card, "r<index>".
func GiftOrderPdfRecipientPart(index int) string {
	return fmt.Sprintf("r%d", index)
}

var giftOrderPdfRecipientPartPattern = regexp.MustCompile(`^r(0|[1-9][0-9]{0,4})$`)

var (
	ErrGiftOrderNotFound   = errors.New("gift order not found")
	ErrGiftOrderNotPending = errors.New("gift order is no longer pending")
	ErrGiftOrderRefunded   = errors.New("gift order was refunded")
)

// The purchase event ids of an order's codes, "<base>/gift/<n>", must fit
// transfer_balance_code.purchase_event_id (varchar(128)), with room for five
// digits of n.
const giftPurchaseEventIdMaxLength = 128
const giftPurchaseEventBaseMaxLength = giftPurchaseEventIdMaxLength - len("/gift/") - 5

// The purchase event id of an order's code n.
func giftPurchaseEventId(base string, n int) string {
	return base + "/gift/" + strconv.Itoa(n)
}

type GiftOrder struct {
	GiftOrderId server.Id
	// the buyer's secret for the order's status and PDF (GetGiftOrderForToken).
	// Set by CreateGiftOrder only: the store keeps its SHA-256, so an order read
	// back has none.
	OrderToken string
	CreateTime time.Time
	Status     string
	PaidTime   *time.Time
	// when a refund ended the order, paid or not
	RefundedTime *time.Time
	Quantity     int
	// USD cents
	UnitPriceCents int64
	TotalCents     int64
	Rail           string
	// set while pending (SetGiftOrderStripeSessionId, SetGiftOrderCryptoIntentId)
	StripeSessionId string
	CryptoIntentId  *server.Id
	Delivery        string
	BuyerEmail      string
	// delivery "them": one address per gift, in code order
	Recipients []string
	// the buyer's local date (a time at 00:00 UTC on that date), sent at 9:00
	// in SendTimezone; nil sends right after payment
	SendOn       *time.Time
	SendTimezone string
	Message      string
	Signature    string
	Language     string
	Season       string
	// the "Made with Bittensor" line on the cards and friend emails
	BittensorFooter bool
	// the signed-in giver's network, whose referral each redeemer becomes
	ReferrerNetworkId *server.Id
	// the combined PDF (part "all") is stored
	PdfReady bool

	// SHA-256 of the order token, as stored
	orderTokenHash []byte
}

// GiftCode is one code of a paid order.
type GiftCode struct {
	BalanceCodeId server.Id
	Secret        string
	// the last moment the code can be redeemed (exclusive)
	RedeemBy time.Time
	// delivery "them": the recipient the code is for (from 0); nil for "me"
	RecipientIndex *int
	Redeemed       bool
	Voided         bool
}

// CreateGiftOrder stores a new pending order with its recipients, and sets the
// order's GiftOrderId, OrderToken (a newCodeBase32 secret), CreateTime and
// Status pending. The caller has validated and priced it. Delivery "them"
// needs exactly Quantity recipients; recipients of delivery "me" are not kept.
// An empty Season is the configured one.
func CreateGiftOrder(ctx context.Context, order *GiftOrder) error {
	if order.Quantity < 1 {
		return fmt.Errorf("gift order quantity %d is below one", order.Quantity)
	}
	recipients := []string{}
	switch order.Delivery {
	case GiftDeliveryMe:
	case GiftDeliveryThem:
		if len(order.Recipients) != order.Quantity {
			return fmt.Errorf("gift order for %d gifts names %d recipients", order.Quantity, len(order.Recipients))
		}
		recipients = order.Recipients
	default:
		return fmt.Errorf("gift order delivery %q is unknown", order.Delivery)
	}
	switch order.Rail {
	case GiftRailStripe, GiftRailCrypto:
	default:
		return fmt.Errorf("gift order rail %q is unknown", order.Rail)
	}
	if order.UnitPriceCents < 0 || order.TotalCents < 0 {
		return fmt.Errorf("gift order price is negative")
	}
	orderToken, err := newCodeBase32()
	if err != nil {
		return err
	}
	season := order.Season
	if season == "" {
		season = Gift().Season
	}
	giftOrderId := server.NewId()
	createTime := server.NowUtc()
	var sendOn *time.Time
	if order.SendOn != nil {
		date := giftOrderDate(*order.SendOn)
		sendOn = &date
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gift_order (
					gift_order_id,
					order_token_hash,
					create_time,
					status,
					quantity,
					unit_price_cents,
					total_cents,
					rail,
					delivery,
					buyer_email,
					send_on,
					send_timezone,
					message,
					signature,
					language,
					season,
					bittensor_footer,
					referrer_network_id
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
			`,
			giftOrderId,
			giftOrderTokenHash(orderToken),
			createTime,
			GiftOrderStatusPending,
			order.Quantity,
			order.UnitPriceCents,
			order.TotalCents,
			order.Rail,
			order.Delivery,
			order.BuyerEmail,
			sendOn,
			order.SendTimezone,
			order.Message,
			order.Signature,
			order.Language,
			season,
			order.BittensorFooter,
			order.ReferrerNetworkId,
		))
		for index, email := range recipients {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO gift_order_recipient (
						gift_order_id,
						recipient_index,
						email
					)
					VALUES ($1, $2, $3)
				`,
				giftOrderId,
				index,
				email,
			))
		}
	})

	order.GiftOrderId = giftOrderId
	order.OrderToken = orderToken
	order.CreateTime = createTime
	order.Status = GiftOrderStatusPending
	order.PaidTime = nil
	order.Season = season
	order.SendOn = sendOn
	order.PdfReady = false
	return nil
}

// The calendar date of t, as a time at 00:00 UTC.
func giftOrderDate(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// The hour, in the buyer's zone, a scheduled delivery goes out on its date.
const GiftOrderDeliverHour = 9

// GiftOrderLocation is the zone an order's dates read in: the buyer's
// (send_timezone), else UTC.
func GiftOrderLocation(timezone string) *time.Location {
	name := strings.TrimSpace(timezone)
	if name == "" || name == "Local" {
		return time.UTC
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return location
}

// GiftOrderDeliverTime is when a scheduled delivery goes out: 9:00 on the
// send-on date (a date, as 00:00 UTC) in the buyer's zone, at the zone's offset
// that day (daylight saving included).
func GiftOrderDeliverTime(sendOn time.Time, timezone string) time.Time {
	date := sendOn.UTC()
	return time.Date(date.Year(), date.Month(), date.Day(), GiftOrderDeliverHour, 0, 0, 0, GiftOrderLocation(timezone)).UTC()
}

// The zone whose day ends last, UTC-12.
var giftRedeemByZone = time.FixedZone("UTC-12", -12*60*60)

// GiftRedeemBy is the end of a code's redeem window that starts at windowStart
// (its delivery, or the payment): redeemable_for later, counted in whole
// calendar years in the buyer's zone when it is a whole number of 365-day years
// (so a year across a 29 February is still a year), then extended to the end of
// that last day in every zone: 00:00 of the next day at UTC-12 (12:00 UTC). The
// date it prints (GiftRedeemByDate) is so a whole day the code still works on
// everywhere, and the window is never shorter than redeemable_for.
func GiftRedeemBy(windowStart time.Time, redeemableFor time.Duration, location *time.Location) time.Time {
	year := 365 * 24 * time.Hour
	end := windowStart.In(location)
	if 0 < redeemableFor && redeemableFor%year == 0 {
		end = end.AddDate(int(redeemableFor/year), 0, 0)
	} else {
		end = end.Add(redeemableFor)
	}
	endYear, endMonth, endDay := end.Date()
	return time.Date(endYear, endMonth, endDay+1, 0, 0, 0, 0, giftRedeemByZone).UTC()
}

// GiftRedeemByDate is the last date a code whose window ends at redeemBy can
// be redeemed on for the whole day in every zone, as 00:00 UTC of that date:
// the date the cards, the emails and the redeem page show ("Redeem by"). A day
// ends last at UTC-12, so it is the day before redeemBy's date there; for a
// GiftRedeemBy cutoff, the window's last day in the buyer's zone. Format it as
// a date in UTC.
func GiftRedeemByDate(redeemBy time.Time) time.Time {
	cutoff := redeemBy.In(giftRedeemByZone)
	return time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day()-1, 0, 0, 0, 0, time.UTC)
}

// SetGiftOrderStripeSessionId keys a pending order to its Stripe Checkout
// session, whose completion fulfils it (GetGiftOrderIdForStripeSession). A
// session id too long to key the order's codes is refused before anyone pays.
func SetGiftOrderStripeSessionId(ctx context.Context, giftOrderId server.Id, sessionId string) error {
	if sessionId == "" {
		return errors.New("gift order stripe session id is empty")
	}
	if giftPurchaseEventBaseMaxLength < len(sessionId) {
		return fmt.Errorf("gift order stripe session id is longer than %d", giftPurchaseEventBaseMaxLength)
	}
	return setGiftOrderPaymentKey(ctx, giftOrderId, "stripe_session_id", sessionId)
}

// SetGiftOrderCryptoIntentId keys a pending order to its crypto invoice,
// whose credit fulfils it (GetGiftOrderIdForCryptoIntent).
func SetGiftOrderCryptoIntentId(ctx context.Context, giftOrderId server.Id, intentId server.Id) error {
	return setGiftOrderPaymentKey(ctx, giftOrderId, "crypto_intent_id", intentId)
}

// Sets the column (a source literal) while the order is pending, or leaves it
// when it already holds the value.
func setGiftOrderPaymentKey(ctx context.Context, giftOrderId server.Id, column string, value any) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			fmt.Sprintf(
				`
					UPDATE gift_order
					SET %[1]s = $2
					WHERE
						gift_order_id = $1 AND
						(status = $3 OR %[1]s IS NOT DISTINCT FROM $2)
				`,
				column,
			),
			giftOrderId,
			value,
			GiftOrderStatusPending,
		))
		if tag.RowsAffected() != 0 {
			return
		}
		if giftOrderExistsInTx(tx, ctx, giftOrderId) {
			returnErr = ErrGiftOrderNotPending
		} else {
			returnErr = ErrGiftOrderNotFound
		}
	})
	return
}

func giftOrderExistsInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) (exists bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM gift_order WHERE gift_order_id = $1)`,
		giftOrderId,
	).Scan(&exists))
	return
}

// GetGiftOrder is the order with its recipients, nil when there is none.
func GetGiftOrder(ctx context.Context, giftOrderId server.Id) (order *GiftOrder) {
	server.Db(ctx, func(conn server.PgConn) {
		order = getGiftOrder(ctx, conn, giftOrderId)
	})
	return
}

func getGiftOrder(ctx context.Context, query server.PgCanQuery, giftOrderId server.Id) *GiftOrder {
	var order *GiftOrder
	result, err := query.Query(
		ctx,
		`
			SELECT
				order_token_hash,
				create_time,
				status,
				paid_time,
				refunded_time,
				quantity,
				unit_price_cents,
				total_cents,
				rail,
				COALESCE(stripe_session_id, ''),
				crypto_intent_id,
				delivery,
				buyer_email,
				send_on,
				send_timezone,
				message,
				signature,
				language,
				season,
				bittensor_footer,
				referrer_network_id,
				EXISTS (
					SELECT 1
					FROM gift_order_pdf
					WHERE
						gift_order_pdf.gift_order_id = gift_order.gift_order_id AND
						gift_order_pdf.part = $2
				)
			FROM gift_order
			WHERE gift_order_id = $1
		`,
		giftOrderId,
		GiftOrderPdfPartAll,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			order = &GiftOrder{
				GiftOrderId: giftOrderId,
			}
			server.Raise(result.Scan(
				&order.orderTokenHash,
				&order.CreateTime,
				&order.Status,
				&order.PaidTime,
				&order.RefundedTime,
				&order.Quantity,
				&order.UnitPriceCents,
				&order.TotalCents,
				&order.Rail,
				&order.StripeSessionId,
				&order.CryptoIntentId,
				&order.Delivery,
				&order.BuyerEmail,
				&order.SendOn,
				&order.SendTimezone,
				&order.Message,
				&order.Signature,
				&order.Language,
				&order.Season,
				&order.BittensorFooter,
				&order.ReferrerNetworkId,
				&order.PdfReady,
			))
		}
	})
	if order == nil {
		return nil
	}

	order.Recipients = []string{}
	result, err = query.Query(
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
	return order
}

// How long an order's token opens it (its status and its cards): past the
// order's codes' window from purchase (gift.redeemable_for, a year) and a
// scheduled delivery, and no longer.
const GiftOrderTokenLifetime = 400 * 24 * time.Hour

// The stored form of an order token: its SHA-256, so a read of the table does
// not open the order.
func giftOrderTokenHash(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}

// GetGiftOrderForToken is GetGiftOrder for the holder of the order's token: the
// token's SHA-256 is compared with the stored one in constant time. nil on a
// mismatch, no order, or an order made more than GiftOrderTokenLifetime ago.
func GetGiftOrderForToken(ctx context.Context, giftOrderId server.Id, token string) *GiftOrder {
	order := GetGiftOrder(ctx, giftOrderId)
	if order == nil || token == "" {
		return nil
	}
	if subtle.ConstantTimeCompare(order.orderTokenHash, giftOrderTokenHash(token)) != 1 {
		return nil
	}
	if order.CreateTime.Add(GiftOrderTokenLifetime).Before(server.NowUtc()) {
		return nil
	}
	return order
}

// GetGiftOrderIdForStripeSession is the order keyed to the session, nil when
// none is: a Checkout session that is not a gift's.
func GetGiftOrderIdForStripeSession(ctx context.Context, sessionId string) *server.Id {
	if sessionId == "" {
		return nil
	}
	return getGiftOrderIdFor(ctx, "stripe_session_id", sessionId)
}

// GetGiftOrderIdForCryptoIntent is the order keyed to the invoice, nil when
// none is.
func GetGiftOrderIdForCryptoIntent(ctx context.Context, intentId server.Id) *server.Id {
	return getGiftOrderIdFor(ctx, "crypto_intent_id", intentId)
}

// The order whose column (a source literal) holds value.
func getGiftOrderIdFor(ctx context.Context, column string, value any) (giftOrderId *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			fmt.Sprintf(`SELECT gift_order_id FROM gift_order WHERE %s = $1`, column),
			value,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&giftOrderId))
			}
		})
	})
	return
}

// The order as fulfilment reads it under its row lock.
type giftOrderFulfilment struct {
	status            string
	quantity          int
	unitPriceCents    int64
	rail              string
	delivery          string
	sendOn            *time.Time
	sendTimezone      string
	buyerEmail        string
	referrerNetworkId *server.Id
	purchaseEventBase *string
	stripeSessionId   *string
	cryptoIntentId    *server.Id
}

// Whether base keys the order's codes where its refund looks for them: the
// order's Stripe session, or "crypto/<intent>" of its invoice
// (CryptoPaymentPurchaseEventId), when the order holds either.
func (self *giftOrderFulfilment) keysPurchaseEvent(base string) bool {
	if self.stripeSessionId == nil && self.cryptoIntentId == nil {
		return true
	}
	return (self.stripeSessionId != nil && *self.stripeSessionId == base) ||
		(self.cryptoIntentId != nil && CryptoPaymentPurchaseEventId(*self.cryptoIntentId) == base)
}

// FulfillGiftOrderInTx fulfils a paid order, in the caller's transaction: it
// creates Quantity pro_gift codes (purchase_event_id "<base>/gift/<n>",
// purchase_email the buyer, the net revenue split between them to the nano
// cent, the referrer from the order, redeemable for gift.redeemable_for from
// the start of now's day), assigns them to the recipients in order (delivery
// "them"), and marks the order paid.
//
// Idempotent under the order's row lock: an order already paid for the same
// purchase event returns its codes with newlyPaid false, so a redelivered
// webhook creates nothing and queues nothing again. The codes are made by the
// existing code-creation path (CreateBalanceCodeInTx), so UNIQUE
// (purchase_event_id) holds for them like every code.
//
// purchaseEventBase is the order's Stripe session id, or
// CryptoPaymentPurchaseEventId of its invoice: the scope its refund claws back
// (ClawbackBalanceCodesForPurchaseEventInTx). A base that is neither, for an
// order keyed to a session or an invoice, is refused.
//
// An error before anything is written (no such order, a refunded order, an
// order paid for another purchase event, no gift config) is returned; the
// caller must not commit a payment it could not fulfil. A failure after the
// first write raises, so the transaction cannot commit half an order.
func FulfillGiftOrderInTx(
	tx server.PgTx,
	ctx context.Context,
	giftOrderId server.Id,
	purchaseEventBase string,
	netRevenueNanoCents int64,
	now time.Time,
) (codes []*GiftCode, newlyPaid bool, err error) {
	if purchaseEventBase == "" || giftPurchaseEventBaseMaxLength < len(purchaseEventBase) {
		return nil, false, fmt.Errorf("gift order purchase event %q is empty or too long", purchaseEventBase)
	}
	if netRevenueNanoCents < 0 {
		return nil, false, fmt.Errorf("gift order net revenue %d is negative", netRevenueNanoCents)
	}
	now = now.UTC()

	var order *giftOrderFulfilment
	result, queryErr := tx.Query(
		ctx,
		`
			SELECT
				status,
				quantity,
				unit_price_cents,
				rail,
				delivery,
				send_on,
				send_timezone,
				buyer_email,
				referrer_network_id,
				purchase_event_base,
				stripe_session_id,
				crypto_intent_id
			FROM gift_order
			WHERE gift_order_id = $1
			FOR UPDATE
		`,
		giftOrderId,
	)
	server.WithPgResult(result, queryErr, func() {
		if result.Next() {
			order = &giftOrderFulfilment{}
			server.Raise(result.Scan(
				&order.status,
				&order.quantity,
				&order.unitPriceCents,
				&order.rail,
				&order.delivery,
				&order.sendOn,
				&order.sendTimezone,
				&order.buyerEmail,
				&order.referrerNetworkId,
				&order.purchaseEventBase,
				&order.stripeSessionId,
				&order.cryptoIntentId,
			))
		}
	})
	if order == nil {
		return nil, false, ErrGiftOrderNotFound
	}
	switch order.status {
	case GiftOrderStatusRefunded:
		return nil, false, ErrGiftOrderRefunded
	case GiftOrderStatusPaid:
		if order.purchaseEventBase == nil || *order.purchaseEventBase != purchaseEventBase {
			return nil, false, fmt.Errorf("gift order %s was fulfilled for another purchase event", giftOrderId)
		}
		return getGiftOrderCodes(ctx, tx, giftOrderId), false, nil
	case GiftOrderStatusPending:
	default:
		return nil, false, fmt.Errorf("gift order %s has the unknown status %q", giftOrderId, order.status)
	}
	if !order.keysPurchaseEvent(purchaseEventBase) {
		// a refund of the order's session or invoice would never find these codes
		return nil, false, fmt.Errorf("gift order %s is not paid by the purchase event %q", giftOrderId, purchaseEventBase)
	}
	if order.quantity < 1 {
		return nil, false, fmt.Errorf("gift order %s has no gifts", giftOrderId)
	}
	config := Gift()
	if config.ProDuration <= 0 || config.RedeemableFor <= 0 {
		// with no pro.yml gift block there is no year to give: refuse loudly
		// rather than make codes that grant nothing or expire on arrival
		return nil, false, fmt.Errorf("gift order %s cannot be fulfilled: pro.yml has no gift pro_duration or redeemable_for", giftOrderId)
	}

	// usable from the start of the purchase day; redeemable for redeemable_for
	// from delivery (GiftRedeemBy): 9:00 on the chosen date in the buyer's zone
	// for delivery "them" with a date, else the payment
	dayStart := giftOrderDate(now)
	windowStart := now
	if order.delivery == GiftDeliveryThem && order.sendOn != nil {
		if deliverTime := GiftOrderDeliverTime(*order.sendOn, order.sendTimezone); now.Before(deliverTime) {
			windowStart = deliverTime
		}
	}
	redeemBy := GiftRedeemBy(windowStart, config.RedeemableFor, GiftOrderLocation(order.sendTimezone))
	proDurationSeconds := int64(config.ProDuration / time.Second)

	revenueBase := netRevenueNanoCents / int64(order.quantity)
	revenueRemainder := netRevenueNanoCents % int64(order.quantity)

	codes = make([]*GiftCode, 0, order.quantity)
	for n := 0; n < order.quantity; n += 1 {
		revenue := revenueBase
		if int64(n) < revenueRemainder {
			revenue += 1
		}
		purchaseRecord, marshalErr := json.Marshal(map[string]any{
			"gift_order_id":    giftOrderId,
			"gift_index":       n,
			"quantity":         order.quantity,
			"unit_price_cents": order.unitPriceCents,
			"rail":             order.rail,
		})
		server.Raise(marshalErr)
		balanceCode, created, createErr := CreateBalanceCodeInTx(
			tx,
			ctx,
			0,
			config.RedeemableFor,
			NanoCents(revenue),
			giftPurchaseEventId(purchaseEventBase, n),
			string(purchaseRecord),
			order.buyerEmail,
		)
		// past the first write: never commit part of an order
		server.Raise(createErr)
		if !created {
			server.Raise(fmt.Errorf("gift order %s code %d: purchase event %s already has a code", giftOrderId, n, balanceCode.PurchaseEventId))
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE transfer_balance_code
				SET
					kind = $2,
					pro_duration_seconds = $3,
					gift_order_id = $4,
					referrer_network_id = $5,
					start_time = $6,
					end_time = $7
				WHERE balance_code_id = $1
			`,
			balanceCode.BalanceCodeId,
			BalanceCodeKindProGift,
			proDurationSeconds,
			giftOrderId,
			order.referrerNetworkId,
			dayStart,
			redeemBy,
		))
		code := &GiftCode{
			BalanceCodeId: balanceCode.BalanceCodeId,
			Secret:        balanceCode.Secret,
			RedeemBy:      redeemBy,
		}
		if order.delivery == GiftDeliveryThem {
			recipientTag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE gift_order_recipient
					SET balance_code_id = $3
					WHERE gift_order_id = $1 AND recipient_index = $2
				`,
				giftOrderId,
				n,
				balanceCode.BalanceCodeId,
			))
			if recipientTag.RowsAffected() != 1 {
				server.Raise(fmt.Errorf("gift order %s has no recipient %d", giftOrderId, n))
			}
			recipientIndex := n
			code.RecipientIndex = &recipientIndex
		}
		codes = append(codes, code)
	}

	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gift_order
			SET
				status = $2,
				paid_time = $3,
				purchase_event_base = $4,
				redeem_by = $5
			WHERE gift_order_id = $1 AND status = $6
		`,
		giftOrderId,
		GiftOrderStatusPaid,
		now,
		purchaseEventBase,
		redeemBy,
		GiftOrderStatusPending,
	))
	return codes, true, nil
}

// GetGiftOrderCodes is the codes of a paid order in code order, with their
// state now; empty while the order is unpaid, nil when there is no order.
func GetGiftOrderCodes(ctx context.Context, giftOrderId server.Id) (codes []*GiftCode) {
	server.Db(ctx, func(conn server.PgConn) {
		codes = getGiftOrderCodes(ctx, conn, giftOrderId)
	})
	return
}

func getGiftOrderCodes(ctx context.Context, query server.PgCanQuery, giftOrderId server.Id) []*GiftCode {
	var found bool
	var quantity int
	var delivery string
	var purchaseEventBase *string
	var redeemBy *time.Time
	result, err := query.Query(
		ctx,
		`
			SELECT quantity, delivery, purchase_event_base, redeem_by
			FROM gift_order
			WHERE gift_order_id = $1
		`,
		giftOrderId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			found = true
			server.Raise(result.Scan(&quantity, &delivery, &purchaseEventBase, &redeemBy))
		}
	})
	if !found {
		return nil
	}
	codes := []*GiftCode{}
	if purchaseEventBase == nil {
		return codes
	}

	// by the purchase event's unique key: an order's codes are exactly these
	purchaseEventIds := make([]string, 0, quantity)
	indexes := map[string]int{}
	for n := 0; n < quantity; n += 1 {
		purchaseEventId := giftPurchaseEventId(*purchaseEventBase, n)
		purchaseEventIds = append(purchaseEventIds, purchaseEventId)
		indexes[purchaseEventId] = n
	}
	byIndex := make([]*GiftCode, quantity)
	result, err = query.Query(
		ctx,
		`
			SELECT
				balance_code_id,
				balance_code_secret,
				purchase_event_id,
				end_time,
				redeem_balance_id IS NOT NULL,
				cancel_time IS NOT NULL
			FROM transfer_balance_code
			WHERE
				purchase_event_id = ANY($1) AND
				gift_order_id = $2
		`,
		purchaseEventIds,
		giftOrderId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			code := &GiftCode{}
			var purchaseEventId string
			var endTime time.Time
			server.Raise(result.Scan(
				&code.BalanceCodeId,
				&code.Secret,
				&purchaseEventId,
				&endTime,
				&code.Redeemed,
				&code.Voided,
			))
			n, ok := indexes[purchaseEventId]
			if !ok {
				continue
			}
			// the code's window until it is redeemed; a redeemed code's window
			// is its Pro year, and its redeem-by the order's
			code.RedeemBy = endTime
			if code.Redeemed && redeemBy != nil {
				code.RedeemBy = *redeemBy
			}
			if delivery == GiftDeliveryThem {
				recipientIndex := n
				code.RecipientIndex = &recipientIndex
			}
			byIndex[n] = code
		}
	})
	for _, code := range byIndex {
		if code != nil {
			codes = append(codes, code)
		}
	}
	return codes
}

// SetGiftOrderRefundedInTx marks an order refunded, in the refund's
// transaction. The refund's clawback (ClawbackBalanceCodesForPurchaseEventInTx
// on the session or invoice) voids its unredeemed codes and ends its redeemed
// codes' grants; this records the order's own state. Marking an order that is
// already refunded changes nothing.
func SetGiftOrderRefundedInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) error {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE gift_order
			SET
				status = $2,
				refunded_time = $3
			WHERE gift_order_id = $1 AND status <> $2
		`,
		giftOrderId,
		GiftOrderStatusRefunded,
		server.NowUtc(),
	))
	if tag.RowsAffected() == 0 && !giftOrderExistsInTx(tx, ctx, giftOrderId) {
		return ErrGiftOrderNotFound
	}
	return nil
}

// PutGiftOrderPdf stores (or replaces) one PDF of an order: part "all", every
// card (which makes the order PdfReady), or "r<index>", recipient index's card.
func PutGiftOrderPdf(ctx context.Context, giftOrderId server.Id, part string, pdf []byte) (returnErr error) {
	if len(pdf) == 0 {
		return errors.New("gift order pdf is empty")
	}
	recipientIndex := -1
	if part != GiftOrderPdfPartAll {
		match := giftOrderPdfRecipientPartPattern.FindStringSubmatch(part)
		if match == nil {
			return fmt.Errorf("gift order pdf part %q is not \"all\" or \"r<index>\"", part)
		}
		recipientIndex, _ = strconv.Atoi(match[1])
	}
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = nil
		var quantity int
		var found bool
		result, err := tx.Query(
			ctx,
			`SELECT quantity FROM gift_order WHERE gift_order_id = $1`,
			giftOrderId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(&quantity))
			}
		})
		if !found {
			returnErr = ErrGiftOrderNotFound
			return
		}
		if quantity <= recipientIndex {
			returnErr = fmt.Errorf("gift order pdf part %q is past the order's %d gifts", part, quantity)
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO gift_order_pdf (
					gift_order_id,
					part,
					pdf,
					create_time
				)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (gift_order_id, part) DO UPDATE
				SET
					pdf = EXCLUDED.pdf,
					create_time = EXCLUDED.create_time
			`,
			giftOrderId,
			part,
			pdf,
			server.NowUtc(),
		))
	})
	return
}

// GetGiftOrderPdf is one stored PDF of an order, nil when it is not stored.
func GetGiftOrderPdf(ctx context.Context, giftOrderId server.Id, part string) (pdf []byte) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT pdf FROM gift_order_pdf WHERE gift_order_id = $1 AND part = $2`,
			giftOrderId,
			part,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&pdf))
			}
		})
	})
	return
}

// RemoveGiftOrderPersonalData is an order's retention (§10.6): its PDFs are
// deleted and its message, signature and recipient emails blanked. The codes
// and the buyer's email, which is every code's purchase email, stay.
func RemoveGiftOrderPersonalData(ctx context.Context, giftOrderId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM gift_order_pdf WHERE gift_order_id = $1`,
			giftOrderId,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gift_order SET message = '', signature = '' WHERE gift_order_id = $1`,
			giftOrderId,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE gift_order_recipient SET email = '' WHERE gift_order_id = $1`,
			giftOrderId,
		))
	})
}

// RemoveGiftOrderBuyerEmail blanks the buyer's address of an order that was
// never paid and that nothing can need again: one paid by card still pending
// (a Stripe Checkout session expires within a day, so it can never be paid),
// or one of either rail refunded before it was ever paid. Retention, 30 days
// after the order was made or refunded, beside RemoveGiftOrderPersonalData. An
// order once paid (refunded since or not) and a pending crypto order (a late
// coin payment can still go to review) keep theirs.
func RemoveGiftOrderBuyerEmail(ctx context.Context, giftOrderId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE gift_order
				SET buyer_email = ''
				WHERE
					gift_order_id = $1 AND
					paid_time IS NULL AND
					(
						(status = $2 AND rail = $3) OR
						status = $4
					)
			`,
			giftOrderId,
			GiftOrderStatusPending,
			GiftRailStripe,
			GiftOrderStatusRefunded,
		))
	})
}

// GiftCodeStatus is what a code is, without redeeming it. Status is ok,
// redeemed, expired, voided (refunded) or not_found. ProDays and RedeemBy are
// set for a gift code; a data code has no redeem-by.
type GiftCodeStatus struct {
	Kind     string
	Status   string
	ProDays  int64
	RedeemBy *time.Time
}

// CheckGiftCode is the status of the code with secret, of either kind.
func CheckGiftCode(ctx context.Context, secret string) *GiftCodeStatus {
	var code *balanceCodeState
	server.Db(ctx, func(conn server.PgConn) {
		code = getBalanceCodeState(ctx, conn, secret)
	})
	return balanceCodeStatusOf(code, server.NowUtc())
}

// A code as the redeem and check paths read it, whatever its state.
type balanceCodeState struct {
	BalanceCode
	kind              string
	proDuration       time.Duration
	giftOrderId       *server.Id
	referrerNetworkId *server.Id
	redeemed          bool
	cancelled         bool
	// the order's redeem-by, for a gift code
	orderRedeemBy *time.Time
}

func (self *balanceCodeState) isGift() bool {
	return self.kind == BalanceCodeKindProGift
}

// The last moment a gift code can be redeemed: its window's end until it is
// redeemed, and the order's after.
func (self *balanceCodeState) redeemBy() *time.Time {
	if !self.isGift() {
		return nil
	}
	if !self.redeemed {
		redeemBy := self.EndTime
		return &redeemBy
	}
	return self.orderRedeemBy
}

func (self *balanceCodeState) proDays() int64 {
	if !self.isGift() {
		return 0
	}
	return giftDurationDays(self.proDuration)
}

// getBalanceCodeState is the code with the secret, whatever its state, nil
// when there is none: the one lookup the redeem, check and check-code paths
// share. It tries the secret's forms in order (balanceCodeSecretCandidates),
// each one probe of the unique secret index, and stops at the first that
// matches: as given, so a legacy secret still matches exactly as it always
// did; then as typed from a card (separators removed, upper case), since every
// code made since newCodeBase32 is 26 upper-case base32 characters, which gift
// cards print in groups; then with the digits base32 never uses read as the
// letters a card's monospace face makes them look like.
func getBalanceCodeState(ctx context.Context, query server.PgCanQuery, secret string) *balanceCodeState {
	for _, candidate := range balanceCodeSecretCandidates(secret) {
		if code := getBalanceCodeStateForSecret(ctx, query, candidate); code != nil {
			return code
		}
	}
	return nil
}

// The forms of a typed secret to look up, in order, each once: the secret as
// given; normalized (normalizeBalanceCodeSecret); and normalized with 0, 1 and
// 8 read as O, I and B (unconfuseBalanceCodeSecret). base32 (A-Z, 2-7) has
// none of those digits, so the last form never turns one valid code into
// another; it comes after the plain normalized form so that a legacy secret
// with digits still matches however it is grouped or cased.
func balanceCodeSecretCandidates(secret string) []string {
	candidates := []string{secret}
	normalized := normalizeBalanceCodeSecret(secret)
	for _, candidate := range []string{normalized, unconfuseBalanceCodeSecret(normalized)} {
		if candidate != "" && !slices.Contains(candidates, candidate) {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

// normalizeBalanceCodeSecret is the secret as a code is stored: without
// whitespace, dashes or dots, upper case.
func normalizeBalanceCodeSecret(secret string) string {
	var normalized strings.Builder
	for _, r := range secret {
		if unicode.IsSpace(r) || unicode.Is(unicode.Pd, r) || r == '.' {
			continue
		}
		normalized.WriteRune(r)
	}
	return strings.ToUpper(normalized.String())
}

// The digits a base32 secret never has, as the letters they pass for in the
// cards' monospace face (Geist Mono): 0 as O, 1 as I, 8 as B.
var balanceCodeSecretLookalikes = strings.NewReplacer("0", "O", "1", "I", "8", "B")

func unconfuseBalanceCodeSecret(secret string) string {
	return balanceCodeSecretLookalikes.Replace(secret)
}

func getBalanceCodeStateForSecret(ctx context.Context, query server.PgCanQuery, secret string) *balanceCodeState {
	var code *balanceCodeState
	result, err := query.Query(
		ctx,
		`
			SELECT
				code.balance_code_id,
				code.balance_code_secret,
				code.start_time,
				code.end_time,
				code.balance_byte_count,
				code.net_revenue_nano_cents,
				code.kind,
				COALESCE(code.pro_duration_seconds, 0),
				code.gift_order_id,
				code.referrer_network_id,
				code.redeem_balance_id IS NOT NULL,
				code.cancel_time IS NOT NULL,
				gift_order.redeem_by
			FROM transfer_balance_code code
			LEFT JOIN gift_order ON gift_order.gift_order_id = code.gift_order_id
			WHERE code.balance_code_secret = $1
		`,
		secret,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			code = &balanceCodeState{}
			var proDurationSeconds int64
			server.Raise(result.Scan(
				&code.BalanceCodeId,
				&code.Secret,
				&code.StartTime,
				&code.EndTime,
				&code.BalanceByteCount,
				&code.NetRevenue,
				&code.kind,
				&proDurationSeconds,
				&code.giftOrderId,
				&code.referrerNetworkId,
				&code.redeemed,
				&code.cancelled,
				&code.orderRedeemBy,
			))
			code.proDuration = time.Duration(proDurationSeconds) * time.Second
		}
	})
	return code
}

// The status of a code at now (nil: no such code). A data code has no
// redeem-by, so it never expires; a refunded code is voided, a refunded code
// that was redeemed stays redeemed.
func balanceCodeStatusOf(code *balanceCodeState, now time.Time) *GiftCodeStatus {
	if code == nil {
		return &GiftCodeStatus{Status: GiftCodeStatusNotFound}
	}
	status := &GiftCodeStatus{
		Kind:     code.kind,
		ProDays:  code.proDays(),
		RedeemBy: code.redeemBy(),
	}
	switch {
	case code.cancelled:
		status.Status = GiftCodeStatusVoided
	case code.redeemed:
		status.Status = GiftCodeStatusRedeemed
	case status.RedeemBy != nil && !now.Before(*status.RedeemBy):
		status.Status = GiftCodeStatusExpired
	default:
		status.Status = GiftCodeStatusOk
	}
	return status
}

// What a refused gift redeem or check says, by status.
func giftCodeStatusMessage(status string) string {
	switch status {
	case GiftCodeStatusRedeemed:
		return balanceCodeRedeemedMessage
	case GiftCodeStatusExpired:
		return balanceCodeExpiredMessage
	case GiftCodeStatusVoided:
		return balanceCodeVoidedMessage
	default:
		return balanceCodeUnknownMessage
	}
}

const (
	balanceCodeUnknownMessage  = "Unknown balance code."
	balanceCodeRedeemedMessage = "Balance code already redeemed."
	balanceCodeExpiredMessage  = "Balance code expired."
	balanceCodeVoidedMessage   = "Balance code was refunded."
)
