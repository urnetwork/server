package controller

// Selling URnetwork Pro gift codes: "Give the Gift of Privacy"
// (mmm/GIFTOFPRIVACY.md §10.2, §10.6 part 2).
//
//	GET  /pay/gift/config         whether gifts are sold, the tiers and the coins (gift_config_controller.go)
//	POST /pay/gift/checkout       a card order: a Stripe Checkout for N codes (this file)
//	POST /pay/gift/crypto-intent  a coin order: a crypto invoice for N codes (gift_crypto_controller.go)
//	POST /pay/gift/order          an order's status, by its id and token (gift_order_controller.go)
//	POST /pay/gift/cards.pdf      an order's cards, by its id and token (gift_order_controller.go)
//	POST /pay/gift/check-code     a code's kind and status, without signing in (gift_order_controller.go)
//
// Every route is public. The purchases take an optional sign-in: with one,
// `referral_credit` links the codes to the caller's network, so a friend who
// redeems one becomes its referral. The server prices every order from the
// pro.yml `gift:` block (model.Gift) and builds the Stripe Checkout and the
// crypto invoice from it; a client never names a price. While the block's
// `enabled` is false every purchase refuses with gift_sales_closed. Refusals
// are in-band, as on the other purchase routes: HTTP 200 with
// {"error": {"code", "message"}}.
//
// A paid order is fulfilled once, in one transaction, however many times its
// payment is reported: model.FulfillGiftOrderInTx makes its codes and marks it
// paid, and when that call is the one that paid it, ScheduleGiftDeliveryInTx
// (the card renderer and the emails) is scheduled in the same transaction. A
// card order is paid by checkout.session.completed (or
// checkout.session.async_payment_succeeded) with metadata gift_order_id; a
// coin order by the crypto rail's one credit (crypto_pay_credit.go). Its codes'
// purchase event ids are "<session id>/gift/<n>" and "crypto/<intent id>/gift/<n>",
// so the refund clawback, which voids every code of a session or an invoice by
// its prefix, voids them on a refund or a dispute, and the order is marked
// refunded with it.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"

	"github.com/stripe/stripe-go/v82"
	stripecheckout "github.com/stripe/stripe-go/v82/checkout/session"
)

// The refusal codes of the gift purchase routes (§10.2), beside the shared
// PurchaseErrorCode* ones: invalid_email, invalid_request (a field a client
// should never send: an unknown delivery, language, ui mode or coin),
// rate_limited, start_failed, checkout_unavailable, and for a coin
// asset_unavailable and item_unavailable.
const (
	// pro.yml `gift.enabled` is false: gifts are not sold now
	GiftErrorCodeSalesClosed = "gift_sales_closed"
	// the quantity is not 1 to gift.max_quantity
	GiftErrorCodeInvalidQuantity = "invalid_quantity"
	// delivery "them" without exactly one valid address per gift
	GiftErrorCodeInvalidRecipients = "invalid_recipients"
	// send_on is not a date from tomorrow to a year out in the buyer's zone,
	// or its zone is not an IANA name
	GiftErrorCodeInvalidSendOn = "invalid_send_on"
	// the note is over 240 characters, or the signature over 40
	GiftErrorCodeMessageTooLong = "message_too_long"
)

// The checkout metadata checkout.session.completed reads back: the order the
// session pays for.
const giftStripeMetadataOrderId = "gift_order_id"

// The one line of a gift checkout, as the customer sees it in Checkout and on
// the receipt.
const giftStripeProductName = "URnetwork Pro, one year (gift code)"

// The gift page. A hosted checkout returns there: with the order when paid,
// so the page can show the paid step (it kept the order's token from the
// checkout's answer), and bare when canceled. Never with the token: a url's
// query lands in access logs and browser history. Fixed server side, like
// stripe.yml's checkout urls: a client-supplied url would let anyone point our
// checkout at their own site.
const giftCheckoutPageUrl = "https://ur.io/gift"

// The share of a card payment Stripe and its costs keep, as every Stripe sku
// in stripe.yml carries it (fee_fraction): the codes' net revenue is the rest.
const giftStripeFeeFraction = 0.3

// The note and the signature, in characters (runes).
const (
	giftMessageMaxRunes   = 240
	giftSignatureMaxRunes = 40
)

// The site languages the cards and the emails are made in (§2.5).
var giftLanguages = []string{"en", "es", "de", "zh", "ru", "ar"}

const giftDefaultLanguage = "en"

// The longest IANA zone name accepted. The longest in the tz database is 32
// characters ("America/Argentina/ComodRivadavia").
const giftTimezoneMaxLength = 64

// -----------------------------------------------------------------------------
// per-ip limits
// -----------------------------------------------------------------------------

// The purchases (a card checkout or a coin invoice) get the data checkout's
// per-address limit, and the reads (the config, an order's status and its
// cards) and the code check the network lookup's; each limiter keeps its own
// counts. In process and fail-open, as payDataIpLimiter is.
var (
	giftPurchaseLimiter  = &payDataIpLimiter{limitPerMinute: payDataCheckoutIpLimitPerMinute}
	giftReadLimiter      = &payDataIpLimiter{limitPerMinute: payDataLookupIpLimitPerMinute}
	giftCheckCodeLimiter = &payDataIpLimiter{limitPerMinute: payDataLookupIpLimitPerMinute}
)

// -----------------------------------------------------------------------------
// the request
// -----------------------------------------------------------------------------

// GiftPurchaseArgs are the fields of a gift order, shared by the card checkout
// and the coin invoice.
type GiftPurchaseArgs struct {
	// how many codes: 1 to gift.max_quantity
	Quantity int `json:"quantity"`
	// "me": every card to `email`; "them": one card to each recipient. Empty
	// is "me".
	Delivery string `json:"delivery"`
	// the buyer: the cards (me) or the receipt (them)
	Email string `json:"email"`
	// delivery "them": exactly `quantity` addresses, the n-th gets the n-th code
	Recipients []string `json:"recipients,omitempty"`
	// delivery "them": the buyer's local date (YYYY-MM-DD) to send the friend
	// emails on, at 9:00 in `timezone`; from tomorrow to a year out. Omitted:
	// right after payment.
	SendOn string `json:"send_on,omitempty"`
	// the buyer's IANA zone; required with send_on
	Timezone string `json:"timezone,omitempty"`
	// the note on the cards, up to 240 characters, and the signature under it,
	// up to 40
	Message   string `json:"message,omitempty"`
	Signature string `json:"signature,omitempty"`
	// en es de zh ru ar: the cards and the emails use it. Empty is en.
	Language string `json:"language,omitempty"`
	// the codes carry the caller's network, so each redeemed code makes its
	// redeemer the network's referral. Honored only with a signed-in session.
	ReferralCredit bool `json:"referral_credit,omitempty"`
}

// GiftCheckoutArgs is POST /pay/gift/checkout.
type GiftCheckoutArgs struct {
	GiftPurchaseArgs
	// "embedded" (Checkout mounts in the dialog and never redirects) or
	// "hosted" (the customer goes to Stripe and comes back to /gift). Empty is
	// hosted.
	UiMode string `json:"ui_mode,omitempty"`
}

// GiftError is a refused gift request: one of the GiftErrorCode* or
// PurchaseErrorCode* values and an English message to fall back to.
type GiftError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// GiftCheckoutResult is POST /pay/gift/checkout.
type GiftCheckoutResult struct {
	// the order, and its token: the bearer secret that reads the order's
	// status and downloads its cards (POST /pay/gift/order, /pay/gift/cards.pdf)
	OrderId    string `json:"order_id,omitempty"`
	OrderToken string `json:"order_token,omitempty"`
	// what the order costs: quantity × the tier's unit price
	TotalCents int64 `json:"total_cents,omitempty"`
	// embedded: what Stripe.js mounts Embedded Checkout with
	ClientSecret   string `json:"client_secret,omitempty"`
	PublishableKey string `json:"publishable_key,omitempty"`
	// hosted: Stripe's checkout url to send the customer to
	Url   string     `json:"url,omitempty"`
	Error *GiftError `json:"error,omitempty"`
}

func giftCheckoutError(code string, message string) *GiftCheckoutResult {
	return &GiftCheckoutResult{Error: &GiftError{Code: code, Message: message}}
}

const giftSalesClosedMessage = "Gift codes are not on sale right now."

// giftValidatePurchase checks a gift order's fields against the config before
// anything is stored, counted against a rate limit or sent to a payment
// provider, and prices it. The order it returns has everything but its rail,
// its referrer and the Bittensor footer, which the caller sets. A refusal is
// its code and its message.
func giftValidatePurchase(
	args *GiftPurchaseArgs,
	giftConfig *model.GiftConfig,
	now time.Time,
) (order *model.GiftOrder, errCode string, errMessage string) {
	quantity := args.Quantity
	if quantity < 1 || giftConfig.MaxQuantity < quantity {
		return nil, GiftErrorCodeInvalidQuantity, fmt.Sprintf("Choose from 1 to %d gifts.", max(giftConfig.MaxQuantity, 1))
	}
	unitPriceCents := giftConfig.UnitPriceCents(quantity)
	if unitPriceCents <= 0 {
		glog.Errorf("[gift]no tier prices %d gifts; is the pro.yml gift block complete?\n", quantity)
		return nil, GiftErrorCodeInvalidQuantity, "That number of gifts is not available."
	}

	delivery := strings.ToLower(strings.TrimSpace(args.Delivery))
	switch delivery {
	case "":
		delivery = model.GiftDeliveryMe
	case model.GiftDeliveryMe, model.GiftDeliveryThem:
	default:
		return nil, PurchaseErrorCodeInvalidRequest, "Unknown delivery."
	}

	email := strings.TrimSpace(args.Email)
	if !payDataValidEmail(email) {
		return nil, PurchaseErrorCodeInvalidEmail, "That email address does not look right."
	}

	// the zone is kept when it is a real one; it is required only to place a
	// send date, so a browser that reports a zone the server does not know
	// can still buy for right after payment
	timezone := strings.TrimSpace(args.Timezone)
	location, timezoneOk := giftLoadLocation(timezone)
	if !timezoneOk {
		timezone = ""
	}

	var recipients []string
	var sendOn *time.Time
	if delivery == model.GiftDeliveryThem {
		if len(args.Recipients) != quantity {
			return nil, GiftErrorCodeInvalidRecipients, "Give one email address for each gift."
		}
		recipients = make([]string, 0, quantity)
		for i, recipient := range args.Recipients {
			recipient = strings.TrimSpace(recipient)
			if !payDataValidEmail(recipient) {
				return nil, GiftErrorCodeInvalidRecipients, fmt.Sprintf("The email address for gift %d does not look right.", i+1)
			}
			recipients = append(recipients, recipient)
		}

		if sendOnText := strings.TrimSpace(args.SendOn); sendOnText != "" {
			if !timezoneOk {
				return nil, GiftErrorCodeInvalidSendOn, "A delivery date needs your time zone."
			}
			date, ok := giftParseSendOn(sendOnText, location, now)
			if !ok {
				return nil, GiftErrorCodeInvalidSendOn, "Choose a delivery date from tomorrow to a year from today."
			}
			sendOn = &date
		}
	}
	// delivery "me" sends every card to the buyer at once: recipients and a
	// send date a client left over from "them" are dropped, not refused

	message := giftCleanText(args.Message, true)
	if giftMessageMaxRunes < utf8.RuneCountInString(message) {
		return nil, GiftErrorCodeMessageTooLong, fmt.Sprintf("The note can be up to %d characters.", giftMessageMaxRunes)
	}
	signature := giftCleanText(args.Signature, false)
	if giftSignatureMaxRunes < utf8.RuneCountInString(signature) {
		return nil, GiftErrorCodeMessageTooLong, fmt.Sprintf("The signature can be up to %d characters.", giftSignatureMaxRunes)
	}

	language := strings.ToLower(strings.TrimSpace(args.Language))
	if language == "" {
		language = giftDefaultLanguage
	}
	if !slices.Contains(giftLanguages, language) {
		return nil, PurchaseErrorCodeInvalidRequest, "Unknown language."
	}

	order = &model.GiftOrder{
		Quantity:       quantity,
		UnitPriceCents: unitPriceCents,
		TotalCents:     unitPriceCents * int64(quantity),
		Delivery:       delivery,
		BuyerEmail:     email,
		Recipients:     recipients,
		SendOn:         sendOn,
		SendTimezone:   timezone,
		Message:        message,
		Signature:      signature,
		Language:       language,
		Season:         giftConfig.Season,
	}
	return order, "", ""
}

// giftLoadLocation is the IANA zone of the name, ok = false for an empty
// name, for "Local" (the server's own zone, which time.LoadLocation would
// return) and for a name the tz database does not have.
func giftLoadLocation(name string) (*time.Location, bool) {
	if name == "" || name == "Local" || giftTimezoneMaxLength < len(name) {
		return nil, false
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, false
	}
	return location, true
}

// giftParseSendOn reads a YYYY-MM-DD local date and checks it is from
// tomorrow to a year from today, both in the buyer's zone. The date is
// returned as midnight UTC of that day: it names a day, not an instant, and
// the delivery reads it with the zone (9:00 there).
func giftParseSendOn(text string, location *time.Location, now time.Time) (time.Time, bool) {
	parsed, err := time.Parse(time.DateOnly, text)
	if err != nil {
		return time.Time{}, false
	}
	date := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC)
	localNow := now.In(location)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)
	yearOut := today.AddDate(1, 0, 0)
	if date.Before(tomorrow) || yearOut.Before(date) {
		return time.Time{}, false
	}
	return date, true
}

// giftCleanText is the buyer's note or signature as it is stored: valid
// UTF-8, without control characters or the bidirectional overrides that can
// make text read differently than it is stored, and trimmed. The note keeps
// its line breaks (\r\n and \r become \n); a signature is one line, so a
// break becomes a space there, since it is written into the friend email's
// subject.
func giftCleanText(text string, multiline bool) string {
	text = strings.ToValidUTF8(text, "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' && multiline:
			b.WriteRune(r)
		case r == '\n' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
		case '‪' <= r && r <= '‮', '⁦' <= r && r <= '⁩':
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// giftReferrerNetworkId is the network the codes credit when redeemed: the
// caller's, when it asked for referral credit and is signed in, else nil.
func giftReferrerNetworkId(args *GiftPurchaseArgs, clientSession *session.ClientSession) *server.Id {
	if !args.ReferralCredit || clientSession == nil || clientSession.ByJwt == nil {
		return nil
	}
	networkId := clientSession.ByJwt.NetworkId
	return &networkId
}

// -----------------------------------------------------------------------------
// the card checkout
// -----------------------------------------------------------------------------

// GiftCheckout answers POST /pay/gift/checkout: the order and a Stripe
// Checkout session for it, embedded or hosted.
func GiftCheckout(
	args *GiftCheckoutArgs,
	clientSession *session.ClientSession,
) (*GiftCheckoutResult, error) {
	giftConfig := model.Gift()
	if !giftConfig.Enabled {
		return giftCheckoutError(GiftErrorCodeSalesClosed, giftSalesClosedMessage), nil
	}
	order, errCode, errMessage := giftValidatePurchase(&args.GiftPurchaseArgs, giftConfig, server.NowUtc())
	if errMessage != "" {
		return giftCheckoutError(errCode, errMessage), nil
	}
	uiMode, ok := stripeCheckoutUiMode(strings.ToLower(strings.TrimSpace(args.UiMode)))
	if !ok {
		return giftCheckoutError(PurchaseErrorCodeInvalidRequest, "Unknown ui mode."), nil
	}

	if !giftPurchaseLimiter.allow(clientSession) {
		return giftCheckoutError(
			PurchaseErrorCodeRateLimited,
			"Too many checkout attempts from this address. Try again in a minute.",
		), nil
	}

	// refuse before an order is stored when this deployment has no Stripe
	apiToken, ok := giftStripeApiToken()
	if !ok {
		glog.Errorf("[gift]stripe is not configured\n")
		return giftCheckoutError(PurchaseErrorCodeCheckoutUnavailable, "Checkout is not configured."), nil
	}

	ctx := clientSession.Ctx
	order.Rail = model.GiftRailStripe
	order.ReferrerNetworkId = giftReferrerNetworkId(&args.GiftPurchaseArgs, clientSession)
	if err := model.CreateGiftOrder(ctx, order); err != nil {
		glog.Errorf("[gift]could not store a gift order: %s\n", err)
		return giftCheckoutError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage), nil
	}

	stripe.Key = apiToken
	checkoutSession, err := stripecheckout.New(giftStripeSessionParams(order, uiMode))
	if err != nil {
		glog.Errorf("[gift]could not create the stripe checkout session of gift order %s: %s\n", order.GiftOrderId, err)
		return giftCheckoutError(PurchaseErrorCodeStartFailed, "Could not start checkout. Please try again."), nil
	}
	if err := model.SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, checkoutSession.ID); err != nil {
		// a session the order is not keyed to is never handed out: its refund
		// could not find the order, and nobody has paid yet
		glog.Errorf("[gift]could not store checkout session %s on gift order %s: %s\n", checkoutSession.ID, order.GiftOrderId, err)
		return giftCheckoutError(PurchaseErrorCodeStartFailed, "Could not start checkout. Please try again."), nil
	}

	result := &GiftCheckoutResult{
		OrderId:    order.GiftOrderId.String(),
		OrderToken: order.OrderToken,
		TotalCents: order.TotalCents,
	}
	switch uiMode {
	case StripeUiModeEmbedded:
		if checkoutSession.ClientSecret == "" {
			glog.Errorf("[gift]embedded checkout session %s has no client secret\n", checkoutSession.ID)
			return giftCheckoutError(PurchaseErrorCodeStartFailed, "Could not start checkout. Please try again."), nil
		}
		result.ClientSecret = checkoutSession.ClientSecret
		result.PublishableKey = stripePublishableKey()
	default:
		if checkoutSession.URL == "" {
			glog.Errorf("[gift]hosted checkout session %s has no url\n", checkoutSession.ID)
			return giftCheckoutError(PurchaseErrorCodeStartFailed, "Could not start checkout. Please try again."), nil
		}
		result.Url = checkoutSession.URL
	}
	glog.Infof(
		"[gift]checkout %s started for gift order %s: %d gifts, %d cents (%s)\n",
		checkoutSession.ID, order.GiftOrderId, order.Quantity, order.TotalCents, uiMode,
	)
	return result, nil
}

// giftStripeApiToken is the Stripe api key, or ok = false when this
// deployment has none (its vault has no stripe.yml).
func giftStripeApiToken() (apiToken string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			apiToken, ok = "", false
		}
	}()
	apiToken = stripeApiTokenFunc()
	return apiToken, apiToken != ""
}

// giftStripeSessionParams is the Checkout session of a stored order (§10.2):
// a one-time payment of one line, the tier's unit price × the quantity, under
// an inline product so the order never depends on a product configured in
// the Stripe dashboard. The webhook finds the order by its metadata.
func giftStripeSessionParams(order *model.GiftOrder, uiMode string) *stripe.CheckoutSessionParams {
	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String(string(stripe.CurrencyUSD)),
					UnitAmount: stripe.Int64(order.UnitPriceCents),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String(giftStripeProductName),
					},
				},
				Quantity: stripe.Int64(int64(order.Quantity)),
			},
		},
		CustomerEmail: stripe.String(order.BuyerEmail),
		// the order's total in usd, never converted to the buyer's currency:
		// the webhook fulfils only that (stripeFulfillGiftCheckout)
		AdaptivePricing: &stripe.CheckoutSessionAdaptivePricingParams{
			Enabled: stripe.Bool(false),
		},
		Metadata: map[string]string{
			giftStripeMetadataOrderId: order.GiftOrderId.String(),
		},
	}
	switch uiMode {
	case StripeUiModeEmbedded:
		// stays in the dialog: Stripe calls the page's onComplete and never
		// navigates, and the dialog follows the order by its token
		params.UIMode = stripe.String(string(stripe.CheckoutSessionUIModeEmbedded))
		params.RedirectOnCompletion = stripe.String("never")
	default:
		params.SuccessURL = stripe.String(giftSuccessUrl(order))
		params.CancelURL = stripe.String(giftCheckoutPageUrl)
	}
	return params
}

// giftSuccessUrl is where a hosted checkout returns once paid:
// https://ur.io/gift?order=<id>, without the order's token.
func giftSuccessUrl(order *model.GiftOrder) string {
	query := url.Values{}
	query.Set("order", order.GiftOrderId.String())
	return giftCheckoutPageUrl + "?" + query.Encode()
}

// -----------------------------------------------------------------------------
// fulfilment: checkout.session.completed
// -----------------------------------------------------------------------------

// stripeGiftCheckout reports a gift order's checkout session: it carries the
// order in its metadata. Such a session never reaches the data pack skus.
func stripeGiftCheckout(checkoutComplete *StripeEventCheckoutCompleteObject) bool {
	return checkoutComplete.Metadata != nil && checkoutComplete.Metadata[giftStripeMetadataOrderId] != ""
}

// stripeFulfillGiftCheckout fulfils the gift order of a paid checkout session
// (the caller checked payment_status): the order's codes, keyed by the
// session id, made once however many deliveries of the session arrive, and
// the order's delivery, scheduled by the delivery that paid it. An error is
// answered non-2xx, so Stripe delivers the event again; what a retry cannot
// change (metadata that names no order of ours) is logged and answered 200.
func stripeFulfillGiftCheckout(
	ctx context.Context,
	checkoutComplete *StripeEventCheckoutCompleteObject,
) error {
	sessionId := checkoutComplete.Id
	giftOrderId, err := server.ParseId(checkoutComplete.Metadata[giftStripeMetadataOrderId])
	if err != nil {
		glog.Errorf("[gift]checkout %s names gift order %q, which is not an id; nothing is fulfilled\n", sessionId, checkoutComplete.Metadata[giftStripeMetadataOrderId])
		return nil
	}
	order := model.GetGiftOrder(ctx, giftOrderId)
	if order == nil {
		// a paid checkout whose order cannot be read: visible in Stripe's
		// dashboard as a failing delivery, retried for 72 h
		return fmt.Errorf("gift order %s of checkout %s was not found", giftOrderId, sessionId)
	}
	if order.Rail != model.GiftRailStripe {
		glog.Errorf("[gift]checkout %s names gift order %s, which is a %s order; nothing is fulfilled\n", sessionId, giftOrderId, order.Rail)
		return nil
	}
	if order.StripeSessionId != "" && order.StripeSessionId != sessionId {
		glog.Errorf("[gift]checkout %s names gift order %s, which belongs to checkout %s; nothing is fulfilled\n", sessionId, giftOrderId, order.StripeSessionId)
		return nil
	}
	if order.Status == model.GiftOrderStatusRefunded {
		glog.Infof("[gift]checkout %s: gift order %s is refunded; nothing is fulfilled\n", sessionId, giftOrderId)
		return nil
	}
	if order.StripeSessionId == "" {
		// not keyed to its session yet (GiftCheckout keys it before handing a
		// session out): the refund handler finds the order by its session
		if err := model.SetGiftOrderStripeSessionId(ctx, giftOrderId, sessionId); err != nil {
			return fmt.Errorf("could not store checkout %s on gift order %s: %w", sessionId, giftOrderId, err)
		}
	}
	if !strings.EqualFold(checkoutComplete.Currency, giftCurrency) || int64(checkoutComplete.AmountTotal) != order.TotalCents {
		// paid something else than the order's total in usd, which the session
		// never asks for: nothing is fulfilled on such a payment's word, and an
		// operator settles it (a refund in Stripe ends the order unfulfilled;
		// the gift store's credit_unfulfillable event alerts)
		return stripeRecordGiftPaymentMismatch(ctx, checkoutComplete, order)
	}
	// the order's own total, which the session charged exactly
	netRevenue := model.UsdToNanoCents((1.0 - giftStripeFeeFraction) * float64(order.TotalCents) / 100.0)
	paymentIntentId := stripeIdFromRaw(checkoutComplete.PaymentIntent)

	now := server.NowUtc()
	var codes []*model.GiftCode
	var newlyPaid bool
	var fulfillErr error
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			codes, newlyPaid, fulfillErr = model.FulfillGiftOrderInTx(tx, ctx, giftOrderId, sessionId, netRevenue, now)
			if fulfillErr != nil {
				// refused before anything was written
				return
			}
			if newlyPaid {
				// the cards and the emails, committed with the codes or not at all
				server.Raise(ScheduleGiftDeliveryInTx(tx, ctx, giftOrderId))
				quantity := len(codes)
				addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
					return giftStripeSalesNotice(giftOrderId, quantity, checkoutComplete, paymentIntentId)
				})
			}
		}, server.TxReadCommitted)
	}, func(err error) {
		fulfillErr = err
	})
	if errors.Is(fulfillErr, model.ErrGiftOrderRefunded) {
		// refunded since it was read above
		glog.Infof("[gift]checkout %s: gift order %s is refunded; nothing is fulfilled\n", sessionId, giftOrderId)
		return nil
	}
	if fulfillErr != nil {
		glog.Errorf("[gift]could not fulfil gift order %s of checkout %s: %s\n", giftOrderId, sessionId, fulfillErr)
		return fulfillErr
	}
	if newlyPaid {
		glog.Infof("[gift]checkout %s paid gift order %s: %d codes\n", sessionId, giftOrderId, len(codes))
	} else {
		glog.Infof("[gift]checkout %s: gift order %s was already fulfilled\n", sessionId, giftOrderId)
	}
	return nil
}

// stripeRecordGiftPaymentMismatch records, once per session, a gift order's
// checkout that paid another currency or amount than the order's total in usd,
// as a gift credit_unfulfillable event (store gift, reason
// gift_payment_mismatch, evidence the session), which the payment-failures
// monitor reports as payment-gift-mismatch (monitor SIGNALS.md §2.22), and
// fulfils nothing. Answers 200 once the record is durable, so Stripe stops
// delivering it.
func stripeRecordGiftPaymentMismatch(
	ctx context.Context,
	checkoutComplete *StripeEventCheckoutCompleteObject,
	order *model.GiftOrder,
) error {
	added, err := model.AddPaymentReconciliationEventOnce(ctx, &model.PaymentReconciliationEvent{
		RunId:    server.NewId(),
		Store:    model.SubscriptionMarketGift,
		Action:   model.PaymentReconcileActionCreditUnfulfillable,
		Evidence: checkoutComplete.Id,
		Details: map[string]any{
			"reason":            "gift_payment_mismatch",
			"leg":               "webhook",
			"gift_order":        order.GiftOrderId.String(),
			"amount_total":      checkoutComplete.AmountTotal,
			"currency":          checkoutComplete.Currency,
			"order_total_cents": order.TotalCents,
		},
	})
	if err != nil {
		return fmt.Errorf("could not record checkout %s's mismatched gift payment: %w", checkoutComplete.Id, err)
	}
	if added {
		glog.Errorf(
			"[gift]checkout %s paid %d %s for gift order %s, whose total is %d usd: not fulfilled, for an operator to settle\n",
			checkoutComplete.Id, checkoutComplete.AmountTotal, checkoutComplete.Currency, order.GiftOrderId, order.TotalCents,
		)
	}
	return nil
}

// giftSalesItem names a gift order in the sales channel.
func giftSalesItem(quantity int) string {
	if quantity == 1 {
		return "1 Pro gift code (one year)"
	}
	return fmt.Sprintf("%d Pro gift codes (one year each)", quantity)
}

// giftStripeSalesNotice is the sales notice of a card gift order.
func giftStripeSalesNotice(
	giftOrderId server.Id,
	quantity int,
	checkoutComplete *StripeEventCheckoutCompleteObject,
	paymentIntentId string,
) *model.SalesPaymentNotice {
	notice := &model.SalesPaymentNotice{
		RecordKey: "gift_order/" + giftOrderId.String(),
		Item:      giftSalesItem(quantity),
		Kind:      model.SalesPaymentKindOneTime,
		Amount:    stripeSalesAmount(int64(checkoutComplete.AmountTotal), string(stripe.CurrencyUSD)),
		Method:    "Stripe",
	}
	if paymentIntentId != "" {
		notice.Enrich = func(ctx context.Context, notice *model.SalesPaymentNotice) {
			if method, ok := stripeSalesPaymentIntentMethod(ctx, paymentIntentId); ok {
				notice.Method = method
			}
		}
	}
	return notice
}
