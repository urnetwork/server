package controller

// Hermetic tests of selling gift codes (gift_*_controller.go): the order's
// validation and its tier prices, the Stripe Checkout params, the coin
// invoice's refusals and its cap, the config's coins, and every refusal that
// happens before a database is read. The database-backed lifecycles (a card
// order through the webhook and its refund, a coin order through its credit)
// are gift_checkout_controller_db_test.go.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// giftPurchaseTestConfig sells gifts with the §10.1 numbers for one test.
func giftPurchaseTestConfig(t testing.TB) *model.GiftConfig {
	config := model.Testing_GiftConfig()
	t.Cleanup(model.Testing_SetGiftConfig(config))
	return config
}

// giftPurchaseTestArgs is a valid order: 3 gifts, all cards to the buyer.
func giftPurchaseTestArgs() GiftPurchaseArgs {
	return GiftPurchaseArgs{
		Quantity:  3,
		Delivery:  model.GiftDeliveryMe,
		Email:     "buyer@example.com",
		Message:   "I'm thinking about you this holiday.",
		Signature: "Sam",
		Language:  "es",
	}
}

func giftPurchaseTestSession() *session.ClientSession {
	return session.Testing_CreateClientSession(context.Background(), nil)
}

// giftPurchaseTestResetLimiters starts the gift routes' per-address windows,
// and the crypto rail's, afresh: every test session shares one address.
func giftPurchaseTestResetLimiters() {
	for _, limiter := range []*payDataIpLimiter{giftPurchaseLimiter, giftReadLimiter, giftCheckCodeLimiter} {
		limiter.stateLock.Lock()
		limiter.window = time.Time{}
		limiter.stateLock.Unlock()
	}
	cryptoTestResetLimiters()
}

// The tiers of §4, from the config the dialog reads: one gift at $39.99, two
// to four at $29.99 each, five to twenty at $23.99 each, and no order of 21.
// Four gifts cost a cent more than five.
func TestGiftPurchaseTierPricing(t *testing.T) {
	config := model.Testing_GiftConfig()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	expectedUnit := func(quantity int) int64 {
		switch {
		case quantity == 1:
			return 3999
		case quantity <= 4:
			return 2999
		default:
			return 2399
		}
	}
	for quantity := 1; quantity <= 20; quantity += 1 {
		args := giftPurchaseTestArgs()
		args.Quantity = quantity
		order, errCode, errMessage := giftValidatePurchase(&args, config, now)
		connect.AssertEqual(t, errCode, "")
		connect.AssertEqual(t, errMessage, "")
		connect.AssertEqual(t, order.UnitPriceCents, expectedUnit(quantity))
		connect.AssertEqual(t, order.TotalCents, expectedUnit(quantity)*int64(quantity))
	}

	four := giftPurchaseTestArgs()
	four.Quantity = 4
	fourOrder, _, _ := giftValidatePurchase(&four, config, now)
	five := giftPurchaseTestArgs()
	five.Quantity = 5
	fiveOrder, _, _ := giftValidatePurchase(&five, config, now)
	connect.AssertEqual(t, fourOrder.TotalCents, int64(11996))
	connect.AssertEqual(t, fiveOrder.TotalCents, int64(11995))

	for _, quantity := range []int{21, 0, -1} {
		args := giftPurchaseTestArgs()
		args.Quantity = quantity
		order, errCode, _ := giftValidatePurchase(&args, config, now)
		connect.AssertEqual(t, order == nil, true)
		connect.AssertEqual(t, errCode, GiftErrorCodeInvalidQuantity)
	}
}

// Every field of §10.2 is checked before anything is stored or sent to
// Stripe, and a valid order keeps exactly what the cards and the emails need.
func TestGiftPurchaseValidation(t *testing.T) {
	config := model.Testing_GiftConfig()
	// 23:30 UTC on 13 November: still the 13th in Chicago (UTC-6), already
	// the 14th in Tokyo (UTC+9)
	now := time.Date(2026, 11, 13, 23, 30, 0, 0, time.UTC)

	refused := func(mutate func(args *GiftPurchaseArgs), code string) {
		t.Helper()
		args := giftPurchaseTestArgs()
		mutate(&args)
		order, errCode, errMessage := giftValidatePurchase(&args, config, now)
		connect.AssertEqual(t, order == nil, true)
		connect.AssertEqual(t, errCode, code)
		connect.AssertEqual(t, errMessage != "", true)
	}
	accepted := func(mutate func(args *GiftPurchaseArgs)) *model.GiftOrder {
		t.Helper()
		args := giftPurchaseTestArgs()
		mutate(&args)
		order, errCode, errMessage := giftValidatePurchase(&args, config, now)
		connect.AssertEqual(t, errCode, "")
		connect.AssertEqual(t, errMessage, "")
		return order
	}
	them := func(args *GiftPurchaseArgs) {
		args.Delivery = model.GiftDeliveryThem
		args.Recipients = []string{"a@example.com", "b@example.com", "c@example.com"}
		args.Timezone = "America/Chicago"
	}

	// the buyer's email: required, and the existing shape check
	refused(func(args *GiftPurchaseArgs) { args.Email = "" }, PurchaseErrorCodeInvalidEmail)
	refused(func(args *GiftPurchaseArgs) { args.Email = "buyer at example" }, PurchaseErrorCodeInvalidEmail)
	refused(func(args *GiftPurchaseArgs) { args.Email = "buyer@localhost" }, PurchaseErrorCodeInvalidEmail)

	// delivery: me or them, nothing else
	refused(func(args *GiftPurchaseArgs) { args.Delivery = "both" }, PurchaseErrorCodeInvalidRequest)

	// them: exactly one valid address per gift
	refused(func(args *GiftPurchaseArgs) {
		them(args)
		args.Recipients = args.Recipients[:2]
	}, GiftErrorCodeInvalidRecipients)
	refused(func(args *GiftPurchaseArgs) {
		them(args)
		args.Recipients = append(args.Recipients, "d@example.com")
	}, GiftErrorCodeInvalidRecipients)
	refused(func(args *GiftPurchaseArgs) {
		them(args)
		args.Recipients[1] = "not an address"
	}, GiftErrorCodeInvalidRecipients)

	// send_on: tomorrow to a year out, in the buyer's own zone
	refused(func(args *GiftPurchaseArgs) { them(args); args.SendOn = "2026-11-13" }, GiftErrorCodeInvalidSendOn)
	refused(func(args *GiftPurchaseArgs) { them(args); args.SendOn = "2027-11-14" }, GiftErrorCodeInvalidSendOn)
	refused(func(args *GiftPurchaseArgs) { them(args); args.SendOn = "2026-12-1" }, GiftErrorCodeInvalidSendOn)
	refused(func(args *GiftPurchaseArgs) { them(args); args.SendOn = "12/24/2026" }, GiftErrorCodeInvalidSendOn)
	refused(func(args *GiftPurchaseArgs) {
		them(args)
		args.SendOn = "2026-12-24"
		args.Timezone = ""
	}, GiftErrorCodeInvalidSendOn)
	refused(func(args *GiftPurchaseArgs) {
		them(args)
		args.SendOn = "2026-12-24"
		args.Timezone = "Mars/Olympus_Mons"
	}, GiftErrorCodeInvalidSendOn)
	// the server's own zone is not the buyer's
	refused(func(args *GiftPurchaseArgs) {
		them(args)
		args.SendOn = "2026-12-24"
		args.Timezone = "Local"
	}, GiftErrorCodeInvalidSendOn)
	// already the 14th in Tokyo: the 14th is today there
	refused(func(args *GiftPurchaseArgs) {
		them(args)
		args.SendOn = "2026-11-14"
		args.Timezone = "Asia/Tokyo"
	}, GiftErrorCodeInvalidSendOn)

	tomorrow := accepted(func(args *GiftPurchaseArgs) { them(args); args.SendOn = "2026-11-14" })
	connect.AssertEqual(t, *tomorrow.SendOn, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, tomorrow.SendTimezone, "America/Chicago")
	yearOut := accepted(func(args *GiftPurchaseArgs) { them(args); args.SendOn = "2027-11-13" })
	connect.AssertEqual(t, *yearOut.SendOn, time.Date(2027, 11, 13, 0, 0, 0, 0, time.UTC))
	tokyo := accepted(func(args *GiftPurchaseArgs) {
		them(args)
		args.SendOn = "2026-11-15"
		args.Timezone = "Asia/Tokyo"
	})
	connect.AssertEqual(t, *tokyo.SendOn, time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))
	// omitted: right after payment
	rightAway := accepted(them)
	connect.AssertEqual(t, rightAway.SendOn == nil, true)
	connect.AssertEqual(t, rightAway.Recipients, []string{"a@example.com", "b@example.com", "c@example.com"})

	// the note and the signature, in characters, not bytes
	refused(func(args *GiftPurchaseArgs) { args.Message = strings.Repeat("中", 241) }, GiftErrorCodeMessageTooLong)
	refused(func(args *GiftPurchaseArgs) { args.Signature = strings.Repeat("é", 41) }, GiftErrorCodeMessageTooLong)
	longest := accepted(func(args *GiftPurchaseArgs) {
		args.Message = strings.Repeat("中", 240)
		args.Signature = strings.Repeat("é", 40)
	})
	connect.AssertEqual(t, longest.Message, strings.Repeat("中", 240))
	connect.AssertEqual(t, longest.Signature, strings.Repeat("é", 40))

	// the six site languages
	refused(func(args *GiftPurchaseArgs) { args.Language = "fr" }, PurchaseErrorCodeInvalidRequest)
	refused(func(args *GiftPurchaseArgs) { args.Language = "pt-BR" }, PurchaseErrorCodeInvalidRequest)
	for _, language := range []string{"en", "es", "de", "zh", "ru", "ar"} {
		order := accepted(func(args *GiftPurchaseArgs) { args.Language = language })
		connect.AssertEqual(t, order.Language, language)
	}
	connect.AssertEqual(t, accepted(func(args *GiftPurchaseArgs) { args.Language = "" }).Language, "en")
	connect.AssertEqual(t, accepted(func(args *GiftPurchaseArgs) { args.Language = " AR " }).Language, "ar")

	// a valid order, as it is stored
	order := accepted(func(args *GiftPurchaseArgs) {
		args.Delivery = " Them "
		args.Email = " buyer@example.com "
		args.Recipients = []string{" a@example.com", "b@example.com ", "c@example.com"}
		args.SendOn = "2026-12-24"
		args.Timezone = "America/Chicago"
		args.Message = "  Line one\r\nline two\u202E  "
		args.Signature = "Sam\nand Alex"
	})
	connect.AssertEqual(t, order.Quantity, 3)
	connect.AssertEqual(t, order.UnitPriceCents, int64(2999))
	connect.AssertEqual(t, order.TotalCents, int64(8997))
	connect.AssertEqual(t, order.Delivery, model.GiftDeliveryThem)
	connect.AssertEqual(t, order.BuyerEmail, "buyer@example.com")
	connect.AssertEqual(t, order.Recipients, []string{"a@example.com", "b@example.com", "c@example.com"})
	connect.AssertEqual(t, order.Message, "Line one\nline two")
	// one line: it is written into the friend email's subject
	connect.AssertEqual(t, order.Signature, "Sam and Alex")
	connect.AssertEqual(t, order.Season, "winter-2026")
	connect.AssertEqual(t, order.Language, "es")

	// delivery me sends every card to the buyer at once: what a client left
	// over from "them" is dropped, not refused
	me := accepted(func(args *GiftPurchaseArgs) {
		args.Delivery = ""
		args.Recipients = []string{"a@example.com"}
		args.SendOn = "not a date"
		args.Timezone = "America/Chicago"
	})
	connect.AssertEqual(t, me.Delivery, model.GiftDeliveryMe)
	connect.AssertEqual(t, len(me.Recipients), 0)
	connect.AssertEqual(t, me.SendOn == nil, true)
	// a zone the server cannot place is dropped when no date needs it
	unknownZone := accepted(func(args *GiftPurchaseArgs) { args.Timezone = "Mars/Olympus_Mons" })
	connect.AssertEqual(t, unknownZone.SendTimezone, "")
}

func TestGiftPurchaseCleanText(t *testing.T) {
	connect.AssertEqual(t, giftCleanText(" a\r\nb\rc\td\x00e\u2066f ", true), "a\nb\nc def")
	connect.AssertEqual(t, giftCleanText(" a\r\nb\rc\td\x00e\u202Af ", false), "a b c def")
	connect.AssertEqual(t, giftCleanText("ok \xff", false), "ok")
	connect.AssertEqual(t, giftCleanText("مع أطيب التمنيات", false), "مع أطيب التمنيات")
}

func TestGiftOrderMaskEmail(t *testing.T) {
	connect.AssertEqual(t, giftMaskEmail("buyer@example.com"), "b•••@example.com")
	connect.AssertEqual(t, giftMaskEmail("é@example.com"), "é•••@example.com")
	connect.AssertEqual(t, giftMaskEmail("@example.com"), "•••@example.com")
	connect.AssertEqual(t, giftMaskEmail("nobody"), "•••")
}

// The Checkout session of §10.2: a one-time payment of one inline line, the
// tier's unit price × the quantity, the order in the metadata, the buyer's
// email, and either the dialog's embedded mount (never redirecting) or the
// hosted pair back to /gift.
func TestGiftCheckoutStripeSessionParams(t *testing.T) {
	giftOrderId := server.NewId()
	order := &model.GiftOrder{
		GiftOrderId:    giftOrderId,
		OrderToken:     "ORDERTOKEN234567ORDERTOKEN",
		Quantity:       3,
		UnitPriceCents: 2999,
		TotalCents:     8997,
		BuyerEmail:     "buyer@example.com",
	}

	params := giftStripeSessionParams(order, StripeUiModeHosted)
	connect.AssertEqual(t, *params.Mode, "payment")
	connect.AssertEqual(t, len(params.LineItems), 1)
	line := params.LineItems[0]
	connect.AssertEqual(t, line.Price == nil, true)
	connect.AssertEqual(t, *line.PriceData.Currency, "usd")
	connect.AssertEqual(t, *line.PriceData.UnitAmount, int64(2999))
	connect.AssertEqual(t, line.PriceData.Product == nil, true)
	connect.AssertEqual(t, *line.PriceData.ProductData.Name, "URnetwork Pro, one year (gift code)")
	connect.AssertEqual(t, *line.Quantity, int64(3))
	connect.AssertEqual(t, *params.CustomerEmail, "buyer@example.com")
	// the total in usd, never converted to the buyer's currency: the webhook
	// fulfils only that
	connect.AssertEqual(t, *params.AdaptivePricing.Enabled, false)
	connect.AssertEqual(t, params.Metadata, map[string]string{"gift_order_id": giftOrderId.String()})
	connect.AssertEqual(t, params.ClientReferenceID == nil, true)
	// the order, never its token: a url's query lands in access logs
	connect.AssertEqual(t, *params.SuccessURL, "https://ur.io/gift?order="+giftOrderId.String())
	connect.AssertEqual(t, *params.CancelURL, "https://ur.io/gift")
	connect.AssertEqual(t, params.UIMode == nil, true)
	connect.AssertEqual(t, params.RedirectOnCompletion == nil, true)

	params = giftStripeSessionParams(order, StripeUiModeEmbedded)
	connect.AssertEqual(t, *params.UIMode, "embedded")
	connect.AssertEqual(t, *params.RedirectOnCompletion, "never")
	// Stripe refuses an embedded session with the hosted pair, or with a
	// return url beside "never"
	connect.AssertEqual(t, params.SuccessURL == nil, true)
	connect.AssertEqual(t, params.CancelURL == nil, true)
	connect.AssertEqual(t, params.ReturnURL == nil, true)
	connect.AssertEqual(t, *params.LineItems[0].Quantity, int64(3))
	connect.AssertEqual(t, params.Metadata["gift_order_id"], giftOrderId.String())

	// the webhook knows the session by its metadata
	connect.AssertEqual(t, stripeGiftCheckout(&StripeEventCheckoutCompleteObject{Metadata: params.Metadata}), true)
	connect.AssertEqual(t, stripeGiftCheckout(&StripeEventCheckoutCompleteObject{Metadata: map[string]string{payDataMetadataItemId: StripeItemData1Tib}}), false)
	connect.AssertEqual(t, stripeGiftCheckout(&StripeEventCheckoutCompleteObject{}), false)
}

// While gift.enabled is false, both purchases refuse with gift_sales_closed,
// whatever the request. And every invalid field is refused by the routes
// themselves before anything is stored.
func TestGiftCheckoutRefusals(t *testing.T) {
	clientSession := giftPurchaseTestSession()

	closed := model.Testing_GiftConfig()
	closed.Enabled = false
	restore := model.Testing_SetGiftConfig(closed)
	checkout, err := GiftCheckout(&GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, checkout.Error.Code, GiftErrorCodeSalesClosed)
	connect.AssertEqual(t, checkout.OrderId, "")
	invoice, err := GiftCryptoIntent(&GiftCryptoIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), Asset: model.CryptoAssetXmr}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, invoice.Error.Code, GiftErrorCodeSalesClosed)
	connect.AssertEqual(t, invoice.OrderId, "")
	// absent from pro.yml is closed too
	model.Testing_SetGiftConfig(&model.GiftConfig{})
	checkout, err = GiftCheckout(&GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, checkout.Error.Code, GiftErrorCodeSalesClosed)
	restore()

	giftPurchaseTestConfig(t)
	checkoutRefused := func(args GiftCheckoutArgs, code string) {
		t.Helper()
		result, err := GiftCheckout(&args, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Code, code)
		connect.AssertEqual(t, result.OrderId, "")
		connect.AssertEqual(t, result.Url, "")
		connect.AssertEqual(t, result.ClientSecret, "")
	}
	args := GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}
	args.Quantity = 21
	checkoutRefused(args, GiftErrorCodeInvalidQuantity)
	args = GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}
	args.Email = "nope"
	checkoutRefused(args, PurchaseErrorCodeInvalidEmail)
	args = GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}
	args.Delivery = model.GiftDeliveryThem
	args.Recipients = []string{"a@example.com"}
	checkoutRefused(args, GiftErrorCodeInvalidRecipients)
	args = GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}
	args.Message = strings.Repeat("x", 241)
	checkoutRefused(args, GiftErrorCodeMessageTooLong)
	args = GiftCheckoutArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), UiMode: "popup"}
	checkoutRefused(args, PurchaseErrorCodeInvalidRequest)

	intentRefused := func(args GiftCryptoIntentArgs, code string) {
		t.Helper()
		result, err := GiftCryptoIntent(&args, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Code, code)
		connect.AssertEqual(t, result.OrderId, "")
		connect.AssertEqual(t, result.IntentId, "")
	}
	intentArgs := GiftCryptoIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), Asset: model.CryptoAssetXmr}
	intentArgs.Quantity = 0
	intentRefused(intentArgs, GiftErrorCodeInvalidQuantity)
	intentArgs = GiftCryptoIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), Asset: model.CryptoAssetXmr}
	intentArgs.Delivery = model.GiftDeliveryThem
	intentArgs.Recipients = []string{"a@example.com", "b@example.com", "c@example.com"}
	intentArgs.SendOn = "2020-01-01"
	intentArgs.Timezone = "UTC"
	intentRefused(intentArgs, GiftErrorCodeInvalidSendOn)
	intentRefused(GiftCryptoIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), Asset: "doge"}, PurchaseErrorCodeInvalidRequest)

	// a coin switched off, and a coin that does not sell codes
	config := cryptoTestConfig()
	config.Assets[model.CryptoAssetXmr].Enabled = false
	codesOff := false
	config.Assets[model.CryptoAssetUsdcSolana].CodeDelivery = &codesOff
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
	intentRefused(GiftCryptoIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), Asset: model.CryptoAssetXmr}, PurchaseErrorCodeAssetUnavailable)
	intentRefused(GiftCryptoIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), Asset: model.CryptoAssetUsdcSolana}, PurchaseErrorCodeAssetUnavailable)
}

// A gift invoice is capped by gift.crypto_max_invoice_usd, not the data
// rail's max_invoice_usd: the cap is the first rule of the quote, before any
// address or rate is read.
func TestGiftCryptoIntentCap(t *testing.T) {
	clientSession := giftPurchaseTestSession()
	cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
	giftPurchaseTestResetLimiters()

	config := model.Testing_GiftConfig()
	config.CryptoMaxInvoiceUsd = 50
	t.Cleanup(model.Testing_SetGiftConfig(config))
	args := GiftCryptoIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), Asset: model.CryptoAssetZec}
	args.Quantity = 2 // $59.98
	result, err := GiftCryptoIntent(&args, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeItemUnavailable)
	connect.AssertEqual(t, result.OrderId, "")

	// no cap set: gift orders are not sold for a coin
	config.CryptoMaxInvoiceUsd = 0
	args.Quantity = 1
	result, err = GiftCryptoIntent(&args, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeItemUnavailable)

	// the gift's own cap replaces the quote's, both ways
	request := &cryptoIntentRequest{asset: model.CryptoAssetZec, item: giftCryptoItem, delivery: model.CryptoDeliveryCode, priceUsd: 479.80, maxInvoiceUsd: 479.79}
	refusal := cryptoStartIntent(clientSession, cryptoPayConfigFunc(), request)
	connect.AssertEqual(t, refusal.Error.Code, PurchaseErrorCodeItemUnavailable)
}

// GET /pay/gift/config hands the dialog the tiers in cents, the year in days
// and the coins that take a gift invoice now: the coins on offer that sell
// codes. Nothing is offered while gifts are not sold.
func TestGiftConfigRoute(t *testing.T) {
	clientSession := giftPurchaseTestSession()
	config := cryptoTestConfig()
	codesOff := false
	config.Assets[model.CryptoAssetXmr].CodeDelivery = &codesOff
	cryptoTestEnable(t, config, map[string]*fakeCryptoWallet{})
	fresh := server.NowUtc().Add(-5 * time.Second)
	height := int64(3_100_000)
	caughtUp := func(asset string) *model.CryptoScanCursor {
		return &model.CryptoScanCursor{Asset: asset, LastSuccessAt: &fresh, WalletHeight: &height, NetworkHeight: &height}
	}
	cryptoTestScanCursors(t, map[string]*model.CryptoScanCursor{
		model.CryptoAssetZec: caughtUp(model.CryptoAssetZec),
		model.CryptoAssetXmr: caughtUp(model.CryptoAssetXmr),
	})

	giftPurchaseTestConfig(t)
	giftPurchaseTestResetLimiters()
	result, err := GiftConfig(clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Enabled, true)
	connect.AssertEqual(t, result.Season, "winter-2026")
	connect.AssertEqual(t, result.Currency, "usd")
	connect.AssertEqual(t, result.MaxQuantity, 20)
	connect.AssertEqual(t, len(result.Tiers), 3)
	connect.AssertEqual(t, *result.Tiers[0], GiftTierResult{MinQuantity: 1, UnitPriceCents: 3999})
	connect.AssertEqual(t, *result.Tiers[1], GiftTierResult{MinQuantity: 2, UnitPriceCents: 2999})
	connect.AssertEqual(t, *result.Tiers[2], GiftTierResult{MinQuantity: 5, UnitPriceCents: 2399})
	connect.AssertEqual(t, result.ProDays, int64(365))
	connect.AssertEqual(t, result.RedeemableDays, int64(365))
	// xmr sells no codes here
	connect.AssertEqual(t, result.CryptoAssets, []string{model.CryptoAssetUsdcSolana, model.CryptoAssetZec})

	closed := model.Testing_GiftConfig()
	closed.Enabled = false
	t.Cleanup(model.Testing_SetGiftConfig(closed))
	result, err = GiftConfig(clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Enabled, false)
	connect.AssertEqual(t, result.CryptoAssets, []string{})
	connect.AssertEqual(t, len(result.Tiers), 3)
}

// An order is read only with its token. What does not name an order, or
// carries no token, is not found, and nothing is read for it.
func TestGiftOrderRefusesWithoutAnOrder(t *testing.T) {
	clientSession := giftPurchaseTestSession()
	for _, args := range []*GiftOrderStatusArgs{
		{OrderId: "nonsense", Token: "ORDERTOKEN"},
		{OrderId: "", Token: ""},
		{OrderId: server.NewId().String(), Token: "  "},
	} {
		status, err := GiftOrderStatus(args, clientSession)
		connect.AssertEqual(t, status == nil, true)
		connect.AssertEqual(t, err, errGiftOrderNotFound)
		pdf, err := GiftCardsPdf(args, clientSession)
		connect.AssertEqual(t, pdf == nil, true)
		connect.AssertEqual(t, err, errGiftOrderNotFound)
	}
	// the refusal is an http 404, not a 403
	connect.AssertEqual(t, strings.HasPrefix(errGiftOrderNotFound.Error(), "404 "), true)

	// a secret that cannot be a code is not found, without a lookup
	result, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: "  "}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Status, model.GiftCodeStatusNotFound)
	result, err = GiftCheckCode(&GiftCheckCodeArgs{Secret: strings.Repeat("A", 65)}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Status, model.GiftCodeStatusNotFound)
}

// A gift order's checkout is fulfilled only once it is paid, and never
// through the data pack skus: an unpaid session and one whose metadata names
// no order are both answered 200 without a call to Stripe.
func TestGiftCheckoutWebhookBeforeTheDatabase(t *testing.T) {
	var stripeCalls atomic.Int64
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stripeCalls.Add(1)
		http.Error(w, "not in the fake", http.StatusNotFound)
	}))
	t.Cleanup(testServer.Close)
	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_gift" }
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
	})
	clientSession := giftPurchaseTestSession()

	unpaid := map[string]any{
		"id":             "cs_test_gift_unpaid",
		"object":         "checkout.session",
		"amount_total":   8997,
		"payment_status": "unpaid",
		"metadata":       map[string]string{"gift_order_id": server.NewId().String()},
	}
	result, err := StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", unpaid), clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error == nil, true)

	malformed := map[string]any{
		"id":             "cs_test_gift_malformed",
		"object":         "checkout.session",
		"amount_total":   8997,
		"payment_status": "paid",
		"metadata":       map[string]string{"gift_order_id": "not an id"},
	}
	result, err = StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", malformed), clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error == nil, true)
	connect.AssertEqual(t, stripeCalls.Load(), int64(0))
}

// A gift invoice's wallet description; and past the retention unlink, which
// forgets its order, a payment to it has nowhere to go, where a data code
// invoice's still credits.
func TestGiftCryptoInvoiceDescription(t *testing.T) {
	connect.AssertEqual(t, cryptoItemDescription(giftCryptoItem), "URnetwork Pro gift codes")
	connect.AssertEqual(t, cryptoGiftInvoice(&model.CryptoPaymentIntent{Item: giftCryptoItem}), true)
	connect.AssertEqual(t, cryptoGiftInvoice(&model.CryptoPaymentIntent{Item: StripeItemData1Tib}), false)

	unlinkedAt := server.NowUtc()
	connect.AssertEqual(t, cryptoIntentUndeliverable(&model.CryptoPaymentIntent{Item: giftCryptoItem, Delivery: model.CryptoDeliveryCode, UnlinkedAt: &unlinkedAt}), true)
	connect.AssertEqual(t, cryptoIntentUndeliverable(&model.CryptoPaymentIntent{Item: giftCryptoItem, Delivery: model.CryptoDeliveryCode}), false)
	connect.AssertEqual(t, cryptoIntentUndeliverable(&model.CryptoPaymentIntent{Item: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode, UnlinkedAt: &unlinkedAt}), false)
}

// An operator's refund of a gift invoice sends back the refunded codes' share
// of what was received and of its credited value, exactly, however large the
// amount: the product is taken in big integers.
func TestCryptoOperatorGiftRefundShare(t *testing.T) {
	credited := model.UsdToNanoCents(479.80)
	intent := &model.CryptoPaymentIntent{Asset: model.CryptoAssetXmr, CreditedUsdNanoCents: &credited}
	gift := cryptoOperatorGiftRefund(intent, 9_000_000_000_000_000_000, &model.GiftCryptoRefund{Quantity: 20, Refunded: 19, Redeemed: 1})
	connect.AssertEqual(t, gift.Quantity, 20)
	connect.AssertEqual(t, gift.Refunded, 19)
	connect.AssertEqual(t, gift.Redeemed, 1)
	connect.AssertEqual(t, gift.RefundAtomic, int64(8_550_000_000_000_000_000))
	connect.AssertEqual(t, gift.Refund, cryptoFormatAtomic(gift.RefundAtomic, cryptoAssetDecimals(model.CryptoAssetXmr)))
	if gift.RefundUsd < 455.80 || 455.82 < gift.RefundUsd {
		t.Fatalf("refund value %f, want 19/20 of 479.80", gift.RefundUsd)
	}

	// a share rounds down, to the atomic unit
	gift = cryptoOperatorGiftRefund(intent, 10, &model.GiftCryptoRefund{Quantity: 3, Refunded: 2})
	connect.AssertEqual(t, gift.RefundAtomic, int64(6))
	// nothing refunded, nothing to send
	gift = cryptoOperatorGiftRefund(&model.CryptoPaymentIntent{Asset: model.CryptoAssetXmr}, 10, &model.GiftCryptoRefund{Quantity: 3})
	connect.AssertEqual(t, gift.RefundAtomic, int64(0))
	connect.AssertEqual(t, gift.RefundUsd, 0.0)
}

// A note or a signature with a link is refused as gift_note_has_link, with a
// message naming which; ordinary prose with dots is not (model.GiftTextHasLink
// holds the whole table).
func TestGiftPurchaseRefusesLinks(t *testing.T) {
	config := model.Testing_GiftConfig()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	validate := func(message string, signature string) (*model.GiftOrder, string, string) {
		args := giftPurchaseTestArgs()
		args.Message = message
		args.Signature = signature
		return giftValidatePurchase(&args, config, now)
	}

	for _, message := range []string{
		"Merry Christmas! See https://example.com",
		"Claim it at www.example.org today",
		"bit.ly/free-pro",
		"Write me: sam@example.com",
		"Join t.me/somechannel",
	} {
		order, errCode, errMessage := validate(message, "Sam")
		connect.AssertEqual(t, order == nil, true)
		connect.AssertEqual(t, errCode, GiftErrorCodeNoteHasLink)
		connect.AssertEqual(t, strings.Contains(errMessage, "note"), true)
	}
	for _, signature := range []string{"sam.example.com", "Sam (example.net)", "HTTPS://X.CO"} {
		order, errCode, errMessage := validate("Happy holidays", signature)
		connect.AssertEqual(t, order == nil, true)
		connect.AssertEqual(t, errCode, GiftErrorCodeNoteHasLink)
		connect.AssertEqual(t, strings.Contains(errMessage, "signature"), true)
	}
	for _, text := range []string{
		"Merry Christmas! 3.5 years of friendship, e.g. the U.S. trip at 10 a.m.",
		"Love you.So much. Te quiero mucho.Es tuyo.",
		"J.R.R. fan forever",
	} {
		order, errCode, errMessage := validate(text, "Ann.Marie")
		connect.AssertEqual(t, errCode, "")
		connect.AssertEqual(t, errMessage, "")
		connect.AssertEqual(t, order.Message, text)
	}
}

// The link rule reads the text as it is stored: a control or bidi character
// removed from inside a domain does not hide it.
func TestGiftPurchaseLinkAfterCleaning(t *testing.T) {
	config := model.Testing_GiftConfig()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, message := range []string{"example\u0000.com", "bit\u202E.ly/x", "www\u2066.example"} {
		args := giftPurchaseTestArgs()
		args.Message = message
		_, errCode, _ := giftValidatePurchase(&args, config, now)
		connect.AssertEqual(t, errCode, GiftErrorCodeNoteHasLink)
	}
	// a line break or a tab still separates, as a space does
	args := giftPurchaseTestArgs()
	args.Message = "Love you\n.com\tpany"
	_, errCode, _ := giftValidatePurchase(&args, config, now)
	connect.AssertEqual(t, errCode, "")
}
