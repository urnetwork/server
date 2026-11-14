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

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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
