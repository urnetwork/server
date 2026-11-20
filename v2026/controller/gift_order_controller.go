package controller

// A gift order after its purchase, and a code before its redemption
// (mmm/GIFTOFPRIVACY.md §10.2, §10.6 part 2):
//
//	POST /pay/gift/order      {order_id, token}: the order's status; the dialog polls it after payment
//	POST /pay/gift/cards.pdf  {order_id, token}: every card of the order, one per page, once rendered
//	POST /pay/gift/check-code {secret}: a code's kind and status, for ur.io/redeem
//
// The order token is the bearer secret of one order, as an invoice's status
// token is: it travels only in request bodies, never in a url, where it would
// land in access logs, and model.GetGiftOrderForToken compares it in constant
// time. A wrong token, an order id that names no order and a malformed one are
// each answered 404, never 403, so an answer says nothing about which order
// ids exist.
//
// /subscription/check-balance-code needs a sign-in; the redeem page checks a
// code before the friend has one, so check-code is public and rate limited per
// address, like the buy-data page's network lookup. A code is a 26-character
// random secret, so a check by guessing finds nothing. A data code that is
// redeemed or refunded reads as not found, as the signed-in check answers it.
//
// The dialog's poll of a card order still pending is the buyer's report of the
// payment, as VerifyStripeCheckout is for a data pack: at most once per order
// every giftCheckoutReportInterval, across the api's processes, the order's
// Checkout session is read from Stripe, and a paid one whose charge was kept
// whole (neither refunded nor disputed) is fulfilled through the webhook's
// own path (stripeFulfillGiftCheckoutInConn), so the buyer sees the order paid
// even when the webhook is late or lost. Whichever of the
// poll, the webhook and the hourly reconciler reports the session first pays
// the order; the others find it paid. A poll that cannot read Stripe answers
// the order as stored.

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// GiftOrderStatusArgs is POST /pay/gift/order and /pay/gift/cards.pdf: the
// order, and its token.
type GiftOrderStatusArgs struct {
	OrderId string `json:"order_id"`
	Token   string `json:"token"`
}

// GiftOrderStatusResult is POST /pay/gift/order.
type GiftOrderStatusResult struct {
	// pending | paid | refunded
	Status   string `json:"status"`
	Quantity int    `json:"quantity"`
	// me | them
	Delivery string `json:"delivery"`
	// delivery them: the local date the friend emails go out (YYYY-MM-DD);
	// absent for right after payment
	SendOn string `json:"send_on,omitempty"`
	// when the friend emails go out (or went out), RFC 3339: the buyer's date
	// at 09:00 their time, else right after payment, and for an order paid by
	// card never before two hours after it (GiftFriendsSendAt); null when no
	// friend is emailed: delivery me, not paid yet, or refunded
	FriendsSendAt *time.Time `json:"friends_send_at"`
	// the buyer's address with the local part hidden: b•••@example.com
	EmailMasked string `json:"email_masked"`
	// the cards are rendered: POST /pay/gift/cards.pdf answers them
	PdfReady bool `json:"pdf_ready"`
}

// GiftCardsPdfResult is POST /pay/gift/cards.pdf: the combined PDF, a card a
// page. The handler writes it as application/pdf.
type GiftCardsPdfResult struct {
	Pdf []byte
}

// The file name the cards download as.
const GiftCardsPdfFileName = "URnetwork-gift-cards.pdf"

// What an order the token does not open answers, whatever the reason.
var errGiftOrderNotFound = errors.New("404 Not found.")

var errGiftReadRateLimited = errors.New("429 Too many requests from this address. Try again in a minute.")

// giftOrderForToken is the order the token opens, or nil.
func giftOrderForToken(clientSession *session.ClientSession, args *GiftOrderStatusArgs) *model.GiftOrder {
	giftOrderId, err := server.ParseId(strings.TrimSpace(args.OrderId))
	token := strings.TrimSpace(args.Token)
	if err != nil || token == "" {
		return nil
	}
	return model.GetGiftOrderForToken(clientSession.Ctx, giftOrderId, token)
}

// GiftOrderStatus answers POST /pay/gift/order.
func GiftOrderStatus(
	args *GiftOrderStatusArgs,
	clientSession *session.ClientSession,
) (*GiftOrderStatusResult, error) {
	if !giftReadLimiter.allow(clientSession) {
		return nil, errGiftReadRateLimited
	}
	order := giftOrderForToken(clientSession, args)
	if order == nil {
		return nil, errGiftOrderNotFound
	}
	order = giftReportCardOrder(clientSession, order)
	result := &GiftOrderStatusResult{
		Status:        order.Status,
		Quantity:      order.Quantity,
		Delivery:      order.Delivery,
		EmailMasked:   giftMaskEmail(order.BuyerEmail),
		PdfReady:      order.PdfReady,
		FriendsSendAt: GiftFriendsSendAt(order),
	}
	if order.SendOn != nil {
		result.SendOn = order.SendOn.UTC().Format(time.DateOnly)
	}
	return result, nil
}

// How often a poll of a pending card order may read its session from Stripe.
const giftCheckoutReportInterval = 20 * time.Second

// How long the poll waits for the report gate before answering without it.
const giftCheckoutReportGateTimeout = 2 * time.Second

// Whether this poll may read the order's session now: the first in
// giftCheckoutReportInterval, across the api's processes. false when the gate
// cannot be read (the poll answers the order as stored). Replaceable by tests.
var giftCheckoutReportGateFunc = func(ctx context.Context, giftOrderId server.Id) (due bool) {
	gateCtx, cancel := context.WithTimeout(ctx, giftCheckoutReportGateTimeout)
	defer cancel()
	server.HandleError(func() {
		server.Redis(gateCtx, func(r server.RedisClient) {
			var err error
			due, err = r.SetNX(gateCtx, "gift_checkout_report_"+giftOrderId.String(), "1", giftCheckoutReportInterval).Result()
			server.Raise(err)
		})
	}, func(err error) {
		due = false
		glog.Infof("[gift]the checkout report gate of gift order %s could not be read: %s\n", giftOrderId, err)
	})
	return
}

// giftReportCardOrder is the order as the buyer's poll answers it: a card
// order still pending whose report is due has its session read from Stripe,
// and is fulfilled through the webhook's path when the session is paid and
// its charge kept whole (see the header). Every other order, an order whose
// session or charge cannot be read, and one whose charge was refunded or
// disputed (the hourly reconciler records it), is answered as stored.
func giftReportCardOrder(clientSession *session.ClientSession, order *model.GiftOrder) *model.GiftOrder {
	if order.Rail != model.GiftRailStripe || order.Status != model.GiftOrderStatusPending || order.StripeSessionId == "" {
		return order
	}
	if _, ok := giftStripeApiToken(); !ok {
		return order
	}
	ctx := clientSession.Ctx
	if !giftCheckoutReportGateFunc(ctx, order.GiftOrderId) {
		return order
	}
	checkoutSession, found, err := stripeReconcileFetchCheckoutSession(ctx, order.StripeSessionId)
	if err != nil || !found {
		glog.Infof("[gift]checkout %s of gift order %s could not be read for its buyer's report (found %t): %v\n", order.StripeSessionId, order.GiftOrderId, found, err)
		return order
	}
	if checkoutSession.Status != "complete" || checkoutSession.Metadata[giftStripeMetadataOrderId] != order.GiftOrderId.String() {
		return order
	}
	checkoutComplete := checkoutSession.completedObject()
	if !stripeCheckoutSessionPaid(checkoutComplete) {
		return order
	}
	// paid is not enough: a refunded or disputed charge is never fulfilled
	// (payment_reconcile_gift.go)
	charge, err := stripeCheckoutSessionCharge(checkoutSession)
	if err != nil {
		glog.Infof("[gift]checkout %s of gift order %s is paid; its charge could not be judged for its buyer's report: %s\n", order.StripeSessionId, order.GiftOrderId, err)
		return order
	}
	if state := giftCheckoutChargeState(charge); state != giftCheckoutChargeKept {
		glog.Infof("[gift]checkout %s of gift order %s is paid and its charge %s is %s: not fulfilled by its buyer's report\n", order.StripeSessionId, order.GiftOrderId, charge.Id, state)
		return order
	}
	outcome, err := stripeFulfillGiftCheckoutInConn(nil, ctx, checkoutComplete, giftCheckoutAudit{leg: "client_report"})
	if err != nil {
		// the webhook and the reconciler still fulfil it
		glog.Errorf("[gift]checkout %s of gift order %s is paid; its buyer's report could not fulfil it: %s\n", order.StripeSessionId, order.GiftOrderId, err)
		return order
	}
	if outcome == giftCheckoutOutcomeFulfilled {
		glog.Infof("[gift]checkout %s: gift order %s was fulfilled by its buyer's report, before its webhook\n", order.StripeSessionId, order.GiftOrderId)
	}
	if reported := model.GetGiftOrder(ctx, order.GiftOrderId); reported != nil {
		return reported
	}
	return order
}

// GiftCardsPdf answers POST /pay/gift/cards.pdf: 404 until the cards are
// rendered, and for a refunded order, whose codes are void.
func GiftCardsPdf(
	args *GiftOrderStatusArgs,
	clientSession *session.ClientSession,
) (*GiftCardsPdfResult, error) {
	if !giftReadLimiter.allow(clientSession) {
		return nil, errGiftReadRateLimited
	}
	order := giftOrderForToken(clientSession, args)
	if order == nil || order.Status == model.GiftOrderStatusRefunded {
		return nil, errGiftOrderNotFound
	}
	pdf := model.GetGiftOrderPdf(clientSession.Ctx, order.GiftOrderId, model.GiftOrderPdfPartAll)
	if len(pdf) == 0 {
		return nil, errGiftOrderNotFound
	}
	return &GiftCardsPdfResult{Pdf: pdf}, nil
}

// giftMaskEmail hides all but the first character of an address's local
// part: buyer@example.com is b•••@example.com.
func giftMaskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return "•••"
	}
	first := ""
	if r, size := utf8.DecodeRuneInString(email[:at]); 0 < size && r != utf8.RuneError {
		first = string(r)
	}
	return first + "•••" + email[at:]
}

// GiftCheckCodeArgs is POST /pay/gift/check-code.
type GiftCheckCodeArgs struct {
	Secret string `json:"secret"`
}

// GiftCheckCodeResult is POST /pay/gift/check-code.
type GiftCheckCodeResult struct {
	// data | pro_gift; empty when no code has the secret
	Kind string `json:"kind,omitempty"`
	// ok | redeemed | expired | voided | not_found
	Status string `json:"status,omitempty"`
	// a gift's year of Pro, in days (365)
	ProDays int64 `json:"pro_days,omitempty"`
	// the end of a gift's redeem window
	RedeemBy *time.Time `json:"redeem_by,omitempty"`
	Error    *GiftError `json:"error,omitempty"`
}

// The longest secret looked up. A code is 26 characters; anything much
// longer is not one.
const giftCheckCodeMaxLength = 64

// GiftCheckCode answers POST /pay/gift/check-code.
func GiftCheckCode(
	args *GiftCheckCodeArgs,
	clientSession *session.ClientSession,
) (*GiftCheckCodeResult, error) {
	secret := strings.TrimSpace(args.Secret)
	if secret == "" || giftCheckCodeMaxLength < len(secret) {
		return &GiftCheckCodeResult{Status: model.GiftCodeStatusNotFound}, nil
	}
	if !giftCheckCodeLimiter.allow(clientSession) {
		return &GiftCheckCodeResult{Error: &GiftError{
			Code:    PurchaseErrorCodeRateLimited,
			Message: "Too many code checks from this address. Try again in a minute.",
		}}, nil
	}
	codeStatus := model.CheckGiftCode(clientSession.Ctx, secret)
	if codeStatus == nil || codeStatus.Status == "" {
		return &GiftCheckCodeResult{Status: model.GiftCodeStatusNotFound}, nil
	}
	// a data code that is redeemed or refunded reads as unknown, as the
	// signed-in check answers it: without a sign-in, nothing says a data code
	// existed. A gift code says which it is, for the redeem page.
	if codeStatus.Kind == model.BalanceCodeKindData &&
		(codeStatus.Status == model.GiftCodeStatusRedeemed || codeStatus.Status == model.GiftCodeStatusVoided) {
		return &GiftCheckCodeResult{Status: model.GiftCodeStatusNotFound}, nil
	}
	return &GiftCheckCodeResult{
		Kind:     codeStatus.Kind,
		Status:   codeStatus.Status,
		ProDays:  codeStatus.ProDays,
		RedeemBy: codeStatus.RedeemBy,
	}, nil
}
