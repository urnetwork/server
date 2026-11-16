package controller

// Database-backed lifecycles of a gift order (gift_*_controller.go): a card
// order from its Checkout session through the webhook, the order's
// token-gated status and cards, the code check and the refund; and a coin
// order from its invoice through the crypto rail's credit and an operator's
// refund. A fake Stripe api stands in for Stripe. Needs the test database
// (server.DefaultTestEnv); the hermetic pieces are
// gift_checkout_controller_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// giftStripeTestApi is a fake Stripe api for a gift order: it makes every
// Checkout session (keeping the form each was made with), and answers the
// charge and the session-by-payment-intent reads of a refund.
type giftStripeTestApi struct {
	stateLock               sync.Mutex
	sessionForms            []url.Values
	charges                 map[string]map[string]any
	sessionsByPaymentIntent map[string][]map[string]any
}

func newGiftStripeTestApi(t testing.TB) *giftStripeTestApi {
	api := &giftStripeTestApi{
		charges:                 map[string]map[string]any{},
		sessionsByPaymentIntent: map[string][]map[string]any{},
	}
	writeJson := func(w http.ResponseWriter, status int, object any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(object)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		api.stateLock.Lock()
		api.sessionForms = append(api.sessionForms, form)
		n := len(api.sessionForms)
		api.stateLock.Unlock()
		sessionId := fmt.Sprintf("cs_test_gift_%d", n)
		object := map[string]any{
			"id":     sessionId,
			"object": "checkout.session",
			"mode":   "payment",
			"status": "open",
		}
		if form.Get("ui_mode") == "embedded" {
			object["ui_mode"] = "embedded"
			object["client_secret"] = sessionId + "_secret_synthetic"
		} else {
			object["ui_mode"] = "hosted"
			object["url"] = "https://checkout.stripe.com/c/pay/" + sessionId
		}
		writeJson(w, http.StatusOK, object)
	})
	mux.HandleFunc("GET /v1/charges/{chargeId}", func(w http.ResponseWriter, r *http.Request) {
		api.stateLock.Lock()
		charge, ok := api.charges[r.PathValue("chargeId")]
		api.stateLock.Unlock()
		if !ok {
			writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "no such charge"}})
			return
		}
		writeJson(w, http.StatusOK, charge)
	})
	mux.HandleFunc("GET /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		api.stateLock.Lock()
		sessions := api.sessionsByPaymentIntent[r.URL.Query().Get("payment_intent")]
		api.stateLock.Unlock()
		if sessions == nil {
			sessions = []map[string]any{}
		}
		writeJson(w, http.StatusOK, map[string]any{"data": sessions, "has_more": false})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "not in the fake"}})
	})
	testServer := httptest.NewServer(mux)

	previousToken := stripeApiToken
	previousPublishableKey := stripePublishableKey
	previousBackend := stripe.GetBackend(stripe.APIBackend)
	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousMessageSender := GetAWSMessageSender()
	stripeApiToken = func() string { return "sk_test_gift" }
	stripePublishableKey = func() string { return "pk_test_gift" }
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(testServer.URL),
		MaxNetworkRetries: stripe.Int64(0),
	}))
	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_gift" }
	SetMessageSender(&mockAWSMessageSender{SendMessageFunc: func(string, Template, ...any) error {
		return nil
	}})
	t.Cleanup(func() {
		stripeApiToken = previousToken
		stripePublishableKey = previousPublishableKey
		stripe.SetBackend(stripe.APIBackend, previousBackend)
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		SetMessageSender(previousMessageSender)
		testServer.Close()
	})
	return api
}

// The form the n-th Checkout session (from 1) was made with.
func (self *giftStripeTestApi) sessionForm(t testing.TB, n int) url.Values {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.sessionForms) < n {
		t.Fatalf("%d checkout sessions were made, want at least %d", len(self.sessionForms), n)
	}
	return self.sessionForms[n-1]
}

func (self *giftStripeTestApi) sessionCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.sessionForms)
}

// The codes stored for an order, voided or not.
func giftPurchaseTestCodeCount(t testing.TB, ctx context.Context, giftOrderId server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT count(*) FROM transfer_balance_code WHERE gift_order_id = $1`,
			giftOrderId,
		).Scan(&count))
	})
	return count
}

// A card order from its Checkout session to its refund: the params sent to
// Stripe, the token-gated status and cards, two deliveries of the paid event
// making one set of codes, the code check, and a refund that voids the codes
// and marks the order refunded.
func TestGiftCardOrderLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		giftPurchaseTestResetLimiters()
		api := newGiftStripeTestApi(t)
		networkId, _, signedIn := cryptoDbTestNetwork(t, ctx, "giftbuyer")
		signedOut := giftPurchaseTestSession()

		// a year out at most, in the buyer's zone (UTC here)
		sendOn := server.NowUtc().AddDate(0, 0, 30).Format(time.DateOnly)

		// hosted, signed in, with referral credit: the codes carry the network
		args := &GiftCheckoutArgs{
			GiftPurchaseArgs: GiftPurchaseArgs{
				Quantity:       3,
				Delivery:       model.GiftDeliveryThem,
				Email:          "buyer@synthetic.example",
				Recipients:     []string{"a@synthetic.example", "b@synthetic.example", "c@synthetic.example"},
				SendOn:         sendOn,
				Timezone:       "UTC",
				Message:        "Pensando en ti.",
				Signature:      "Sam",
				Language:       "es",
				ReferralCredit: true,
			},
			UiMode: StripeUiModeHosted,
		}
		hosted, err := GiftCheckout(args, signedIn)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, hosted.Error == nil, true)
		connect.AssertEqual(t, hosted.TotalCents, int64(8997))
		connect.AssertEqual(t, hosted.Url, "https://checkout.stripe.com/c/pay/cs_test_gift_1")
		connect.AssertEqual(t, hosted.ClientSecret, "")
		connect.AssertEqual(t, hosted.OrderToken != "", true)

		form := api.sessionForm(t, 1)
		connect.AssertEqual(t, form.Get("mode"), "payment")
		connect.AssertEqual(t, form.Get("line_items[0][price_data][currency]"), "usd")
		connect.AssertEqual(t, form.Get("line_items[0][price_data][unit_amount]"), "2999")
		connect.AssertEqual(t, form.Get("line_items[0][price_data][product_data][name]"), "URnetwork Pro, one year (gift code)")
		connect.AssertEqual(t, form.Get("line_items[0][quantity]"), "3")
		connect.AssertEqual(t, form.Has("line_items[1][quantity]"), false)
		connect.AssertEqual(t, form.Has("line_items[0][price]"), false)
		connect.AssertEqual(t, form.Get("metadata[gift_order_id]"), hosted.OrderId)
		connect.AssertEqual(t, form.Get("customer_email"), "buyer@synthetic.example")
		// the order, never its token: a url's query lands in access logs
		connect.AssertEqual(t, form.Get("success_url"), "https://ur.io/gift?order="+hosted.OrderId)
		for key, values := range form {
			for _, value := range values {
				if strings.Contains(value, hosted.OrderToken) {
					t.Fatalf("the checkout params carry the order token in %s", key)
				}
			}
		}
		connect.AssertEqual(t, form.Get("cancel_url"), "https://ur.io/gift")
		connect.AssertEqual(t, form.Has("ui_mode"), false)
		// the order's total in usd, never converted to the buyer's currency
		connect.AssertEqual(t, form.Get("adaptive_pricing[enabled]"), "false")
		// never a client reference: the fulfilment must not read the buyer's
		// network as the destination of a data purchase
		connect.AssertEqual(t, form.Has("client_reference_id"), false)

		giftOrderId := server.RequireParseId(hosted.OrderId)
		order := model.GetGiftOrder(ctx, giftOrderId)
		connect.AssertEqual(t, order.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, order.Rail, model.GiftRailStripe)
		connect.AssertEqual(t, order.StripeSessionId, "cs_test_gift_1")
		connect.AssertEqual(t, order.Quantity, 3)
		connect.AssertEqual(t, order.UnitPriceCents, int64(2999))
		connect.AssertEqual(t, order.TotalCents, int64(8997))
		connect.AssertEqual(t, order.Recipients, args.Recipients)
		connect.AssertEqual(t, order.SendOn.Format(time.DateOnly), sendOn)
		connect.AssertEqual(t, order.SendTimezone, "UTC")
		connect.AssertEqual(t, order.Language, "es")
		connect.AssertEqual(t, order.BittensorFooter, false)
		connect.AssertEqual(t, *order.ReferrerNetworkId, networkId)

		// embedded and signed out: referral credit is not honored without a
		// sign-in, and the session stays in the dialog
		embeddedArgs := &GiftCheckoutArgs{
			GiftPurchaseArgs: GiftPurchaseArgs{
				Quantity:       1,
				Email:          "other@synthetic.example",
				ReferralCredit: true,
			},
			UiMode: StripeUiModeEmbedded,
		}
		embedded, err := GiftCheckout(embeddedArgs, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, embedded.Error == nil, true)
		connect.AssertEqual(t, embedded.TotalCents, int64(3999))
		connect.AssertEqual(t, embedded.ClientSecret, "cs_test_gift_2_secret_synthetic")
		connect.AssertEqual(t, embedded.PublishableKey, "pk_test_gift")
		connect.AssertEqual(t, embedded.Url, "")
		form = api.sessionForm(t, 2)
		connect.AssertEqual(t, form.Get("ui_mode"), "embedded")
		connect.AssertEqual(t, form.Get("redirect_on_completion"), "never")
		connect.AssertEqual(t, form.Has("success_url"), false)
		connect.AssertEqual(t, form.Has("cancel_url"), false)
		connect.AssertEqual(t, form.Has("return_url"), false)
		connect.AssertEqual(t, form.Get("line_items[0][price_data][unit_amount]"), "3999")
		connect.AssertEqual(t, form.Get("line_items[0][quantity]"), "1")
		embeddedOrder := model.GetGiftOrder(ctx, server.RequireParseId(embedded.OrderId))
		connect.AssertEqual(t, embeddedOrder.ReferrerNetworkId == nil, true)
		connect.AssertEqual(t, embeddedOrder.Delivery, model.GiftDeliveryMe)
		connect.AssertEqual(t, embeddedOrder.Language, "en")

		// signed in without asking for it: no referral either
		args.ReferralCredit = false
		noCredit, err := GiftCheckout(args, signedIn)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, server.RequireParseId(noCredit.OrderId)).ReferrerNetworkId == nil, true)
		connect.AssertEqual(t, api.sessionCount(), 3)

		// the order's status, only with its token
		statusArgs := &GiftOrderStatusArgs{OrderId: hosted.OrderId, Token: hosted.OrderToken}
		status, err := GiftOrderStatus(statusArgs, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *status, GiftOrderStatusResult{
			Status:      model.GiftOrderStatusPending,
			Quantity:    3,
			Delivery:    model.GiftDeliveryThem,
			SendOn:      sendOn,
			EmailMasked: "b•••@synthetic.example",
			PdfReady:    false,
		})
		for _, refused := range []*GiftOrderStatusArgs{
			{OrderId: hosted.OrderId, Token: embedded.OrderToken},
			{OrderId: hosted.OrderId, Token: strings.ToLower(hosted.OrderToken)},
			{OrderId: server.NewId().String(), Token: hosted.OrderToken},
		} {
			_, err = GiftOrderStatus(refused, signedOut)
			connect.AssertEqual(t, err, errGiftOrderNotFound)
			_, err = GiftCardsPdf(refused, signedOut)
			connect.AssertEqual(t, err, errGiftOrderNotFound)
		}
		// no cards before they are rendered
		_, err = GiftCardsPdf(statusArgs, signedOut)
		connect.AssertEqual(t, err, errGiftOrderNotFound)

		completed := map[string]any{
			"id":             "cs_test_gift_1",
			"object":         "checkout.session",
			"mode":           "payment",
			"amount_total":   8997,
			"currency":       "usd",
			"payment_status": "paid",
			"payment_intent": "pi_test_gift_1",
			"metadata":       map[string]string{"gift_order_id": hosted.OrderId},
			"customer_details": map[string]any{
				"email": "buyer@synthetic.example",
			},
		}
		webhookSession := reconcileTestSession(t, ctx)

		// paid another amount, or in another currency, than the order's total
		// in usd: nothing is fulfilled, and an operator is told once
		mismatched := func(field string, value any) map[string]any {
			event := map[string]any{}
			for key, existing := range completed {
				event[key] = existing
			}
			event[field] = value
			return event
		}
		for _, event := range []map[string]any{
			mismatched("amount_total", 8996),
			mismatched("currency", "eur"),
			mismatched("currency", ""),
		} {
			_, err = StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", event), webhookSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, giftOrderId), 0)
		}
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusPending)
		// recorded once, under the gift store, for the payment-failures alert
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketGift, model.PaymentReconcileActionCreditUnfulfillable, "cs_test_gift_1"),
			1,
		)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable, "cs_test_gift_1"),
			0,
		)

		// Stripe reports the paid session twice: one set of codes
		for range 2 {
			_, err = StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", completed), webhookSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, giftOrderId), 3)
		}
		// and once more as the delayed-payment event: still the same codes
		_, err = StripeWebhook(stripeWebhookEvent(t, "checkout.session.async_payment_succeeded", completed), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, giftOrderId), 3)

		order = model.GetGiftOrder(ctx, giftOrderId)
		connect.AssertEqual(t, order.Status, model.GiftOrderStatusPaid)
		codes := model.GetGiftOrderCodes(ctx, giftOrderId)
		connect.AssertEqual(t, len(codes), 3)
		for n, code := range codes {
			connect.AssertEqual(t, code.Voided, false)
			connect.AssertEqual(t, code.Redeemed, false)
			connect.AssertEqual(t, *code.RecipientIndex, n)
			balanceCode, err := model.GetBalanceCode(ctx, code.BalanceCodeId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, balanceCode.PurchaseEventId, fmt.Sprintf("cs_test_gift_1/gift/%d", n))
		}
		// the paid order is never fulfilled again by another of our sessions
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, server.RequireParseId(embedded.OrderId)), 0)

		// the redeem page's check, without a sign-in
		checked, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: " " + codes[0].Secret + " "}, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Kind, model.BalanceCodeKindProGift)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusOk)
		connect.AssertEqual(t, checked.ProDays, int64(365))
		connect.AssertEqual(t, checked.RedeemBy != nil, true)
		connect.AssertEqual(t, checked.RedeemBy.After(server.NowUtc().AddDate(0, 11, 0)), true)
		checked, err = GiftCheckCode(&GiftCheckCodeArgs{Secret: "AAAAAAAAAAAAAAAAAAAAAAAAAA"}, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusNotFound)

		// the cards, once rendered, only with the token
		pdf := []byte("%PDF-1.7 synthetic gift cards")
		connect.AssertEqual(t, model.PutGiftOrderPdf(ctx, giftOrderId, model.GiftOrderPdfPartAll, pdf), nil)
		status, err = GiftOrderStatus(statusArgs, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, status.PdfReady, true)
		cards, err := GiftCardsPdf(statusArgs, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cards.Pdf, pdf)
		_, err = GiftCardsPdf(&GiftOrderStatusArgs{OrderId: hosted.OrderId, Token: noCredit.OrderToken}, signedOut)
		connect.AssertEqual(t, err, errGiftOrderNotFound)

		// a refund of the whole charge voids every unredeemed code (here all
		// of them) and marks the order refunded, in the refund's transaction
		api.stateLock.Lock()
		api.charges["ch_test_gift_1"] = map[string]any{
			"id":             "ch_test_gift_1",
			"payment_intent": "pi_test_gift_1",
			"amount":         8997,
		}
		api.sessionsByPaymentIntent["pi_test_gift_1"] = []map[string]any{{"id": "cs_test_gift_1"}}
		api.stateLock.Unlock()
		refund := map[string]any{"id": "re_test_gift_1", "charge": "ch_test_gift_1", "amount": 8997}
		_, err = StripeWebhook(stripeWebhookEvent(t, "refund.created", refund), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(
			t,
			countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionRefunded, "re_test_gift_1"),
			1,
		)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusRefunded)
		for _, code := range model.GetGiftOrderCodes(ctx, giftOrderId) {
			connect.AssertEqual(t, code.Voided, true)
		}
		checked, err = GiftCheckCode(&GiftCheckCodeArgs{Secret: codes[0].Secret}, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusVoided)
		status, err = GiftOrderStatus(statusArgs, signedOut)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, model.GiftOrderStatusRefunded)
		// a refunded order's cards are not served: their codes are void
		_, err = GiftCardsPdf(statusArgs, signedOut)
		connect.AssertEqual(t, err, errGiftOrderNotFound)

		// the refund delivered again, and the paid event after it: nothing
		// more is voided or made
		_, err = StripeWebhook(stripeWebhookEvent(t, "refund.created", refund), webhookSession)
		connect.AssertEqual(t, err, nil)
		_, err = StripeWebhook(stripeWebhookEvent(t, "checkout.session.completed", completed), webhookSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, giftOrderId), 3)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusRefunded)
	})
}

// A coin order: its invoice is priced at the order's total and capped by
// gift.crypto_max_invoice_usd (20 gifts, $479.80, over the data rail's $100);
// the payment's credit makes the order's codes once under the invoice's
// prefix, records the first as the invoice's grant and never shows a code in
// the invoice's status; and an operator's refund voids every code and marks
// the order refunded.
func TestGiftCryptoOrderLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		giftPurchaseTestResetLimiters()
		cryptoTestStandardRates(t)
		wallet := newFakeCryptoWallet(model.CryptoAssetZec)
		cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{model.CryptoAssetZec: wallet})
		// the refund destination's screen, without OFAC's table
		cryptoTestSanctions(t, map[string]bool{}, nil)
		pageSession := session.Testing_CreateClientSession(ctx, nil)
		taskSession := session.Testing_CreateClientSession(ctx, nil)
		previousSender := GetAWSMessageSender()
		SetMessageSender(&mockAWSMessageSender{SendMessageFunc: func(string, Template, ...any) error {
			return nil
		}})
		t.Cleanup(func() { SetMessageSender(previousSender) })

		_, err := RunCryptoAddressRefill(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)
		resetCryptoAvailability()

		args := &GiftCryptoIntentArgs{
			GiftPurchaseArgs: GiftPurchaseArgs{
				Quantity:  20,
				Email:     "buyer@synthetic.example",
				Message:   "For the team.",
				Signature: "Sam",
				Language:  "de",
			},
			Asset:           model.CryptoAssetZec,
			BittensorFooter: true,
		}
		invoice, err := GiftCryptoIntent(args, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, invoice.Error == nil, true)
		connect.AssertEqual(t, invoice.ItemId, giftCryptoItem)
		connect.AssertEqual(t, invoice.Plan, "")
		connect.AssertEqual(t, invoice.Delivery, model.CryptoDeliveryCode)
		connect.AssertEqual(t, invoice.PriceUsd, 479.80)
		connect.AssertEqual(t, invoice.BittensorFooter, true)
		connect.AssertEqual(t, invoice.OrderId != "", true)
		connect.AssertEqual(t, invoice.OrderToken != "", true)
		connect.AssertEqual(t, invoice.StatusToken != "", true)
		connect.AssertEqual(t, strings.Contains(invoice.Uri, "message=URnetwork%20Pro%20gift%20codes"), true)

		giftOrderId := server.RequireParseId(invoice.OrderId)
		intentId := server.RequireParseId(invoice.IntentId)
		order := model.GetGiftOrder(ctx, giftOrderId)
		connect.AssertEqual(t, order.Rail, model.GiftRailCrypto)
		connect.AssertEqual(t, *order.CryptoIntentId, intentId)
		connect.AssertEqual(t, order.TotalCents, int64(47980))
		connect.AssertEqual(t, order.BittensorFooter, true)
		connect.AssertEqual(t, order.ReferrerNetworkId == nil, true)
		connect.AssertEqual(t, *model.GetGiftOrderIdForCryptoIntent(ctx, intentId), giftOrderId)

		// paid on the chain, credited by the scanner
		intent := model.GetCryptoPaymentIntent(ctx, intentId)
		connect.AssertEqual(t, intent.Item, giftCryptoItem)
		connect.AssertEqual(t, intent.Email, "")
		wallet.pay("tx-gift", uint64(*intent.AddressIndex), uint64(intent.AmountAtomic))
		wallet.mine("tx-gift", 991)
		_, err = RunCryptoPaymentScan(taskSession, model.CryptoAssetZec)
		connect.AssertEqual(t, err, nil)

		order = model.GetGiftOrder(ctx, giftOrderId)
		connect.AssertEqual(t, order.Status, model.GiftOrderStatusPaid)
		codes := model.GetGiftOrderCodes(ctx, giftOrderId)
		connect.AssertEqual(t, len(codes), 20)
		for n, code := range codes {
			balanceCode, err := model.GetBalanceCode(ctx, code.BalanceCodeId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, balanceCode.PurchaseEventId, fmt.Sprintf("crypto/%s/gift/%d", intentId, n))
			connect.AssertEqual(t, code.RecipientIndex == nil, true)
		}
		credited := model.GetCryptoPaymentIntent(ctx, intentId)
		connect.AssertEqual(t, credited.CreditedAt != nil, true)
		connect.AssertEqual(t, *credited.BalanceCodeId, codes[0].BalanceCodeId)
		connect.AssertEqual(t, credited.BalanceId == nil, true)

		// the invoice reads paid, and never shows one of the order's codes
		status, err := CryptoPaymentStatus(&CryptoPaymentStatusArgs{IntentId: invoice.IntentId, StatusToken: invoice.StatusToken}, pageSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Status, CryptoPaymentStatusPaid)
		connect.AssertEqual(t, status.BalanceCode, "")

		// a credit after the fact makes nothing
		again, err := cryptoCreditIntent(taskSession, credited, &cryptoCredit{usdNanoCents: credited.PriceUsdNanoCents})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again, false)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, giftOrderId), 20)

		// two credits of another order's invoice at once credit it once, and
		// an operator's refund voids its codes and marks the order refunded
		xmrOrder := &model.GiftOrder{
			Quantity:       3,
			UnitPriceCents: 2999,
			TotalCents:     8997,
			Rail:           model.GiftRailCrypto,
			Delivery:       model.GiftDeliveryMe,
			BuyerEmail:     "buyer@synthetic.example",
			Language:       "en",
		}
		connect.AssertEqual(t, model.CreateGiftOrder(ctx, xmrOrder), nil)
		xmrIntent := cryptoDbCreateIntent(t, ctx, model.CryptoAssetXmr, nil, giftCryptoItem, 89.97, 801)
		connect.AssertEqual(t, model.SetGiftOrderCryptoIntentId(ctx, xmrOrder.GiftOrderId, xmrIntent.IntentId), nil)
		receipt := cryptoDbReceipt(t, ctx, xmrIntent, "tx-gift-xmr", xmrIntent.AmountAtomic, 4)
		credit := func() *cryptoCredit {
			return &cryptoCredit{
				usdNanoCents: xmrIntent.PriceUsdNanoCents,
				receipts:     []model.CryptoPaymentReceiptKey{receipt.Key()},
				receiptUsd:   map[model.CryptoPaymentReceiptKey]model.NanoCents{receipt.Key(): xmrIntent.PriceUsdNanoCents},
			}
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		results := make([]bool, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				results[i], errs[i] = cryptoCreditIntent(taskSession, xmrIntent, credit())
			}()
		}
		close(start)
		wait.Wait()
		connect.AssertEqual(t, errs[0], nil)
		connect.AssertEqual(t, errs[1], nil)
		connect.AssertEqual(t, results[0] != results[1], true)
		connect.AssertEqual(t, giftPurchaseTestCodeCount(t, ctx, xmrOrder.GiftOrderId), 3)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, xmrOrder.GiftOrderId).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, model.GetCryptoPaymentReceiptsForIntent(ctx, xmrIntent.IntentId)[0].State, model.CryptoReceiptStateCredited)

		payout := "5" + fmt.Sprintf("%094d", 7)
		refund, err := CryptoOperatorRefund(taskSession, xmrIntent.IntentId, payout)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(refund.EndedNetworkIds), 0)
		connect.AssertEqual(t, refund.Gift.Refunded, 3)
		connect.AssertEqual(t, refund.Gift.RefundAtomic, refund.ReceivedAtomic)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, xmrOrder.GiftOrderId).Status, model.GiftOrderStatusRefunded)
		for _, code := range model.GetGiftOrderCodes(ctx, xmrOrder.GiftOrderId) {
			connect.AssertEqual(t, code.Voided, true)
		}
		// the zec order is untouched
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusPaid)
		for _, code := range model.GetGiftOrderCodes(ctx, giftOrderId) {
			connect.AssertEqual(t, code.Voided, false)
		}
	})
}

// The public code check says what a gift code is, whatever its state, for the
// redeem page; a data code it shows only while it can be redeemed: redeemed
// or refunded, a data code reads as unknown, as the signed-in check answers
// it, without its kind.
func TestGiftCheckCodeHidesSpentDataCodes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		giftPurchaseTestConfig(t)
		giftPurchaseTestResetLimiters()
		// the data codes' emails go to the fake Stripe env's sender
		newStripeRefundTestEnv(t)
		signedOut := session.Testing_CreateClientSession(ctx, nil)

		dataSecret := func(sessionId string) (server.Id, string) {
			err := CreateBalanceCode(ctx, 1*model.Tib, 30*24*time.Hour, model.UsdToNanoCents(5.00), sessionId, "test-record", "buyer@synthetic.example", nil)
			connect.AssertEqual(t, err, nil)
			balanceCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, sessionId)
			connect.AssertEqual(t, err, nil)
			balanceCode, err := model.GetBalanceCode(ctx, balanceCodeId)
			connect.AssertEqual(t, err, nil)
			return balanceCodeId, balanceCode.Secret
		}
		check := func(secret string) *GiftCheckCodeResult {
			checked, err := GiftCheckCode(&GiftCheckCodeArgs{Secret: secret}, signedOut)
			connect.AssertEqual(t, err, nil)
			return checked
		}

		// redeemable: ok, a data code
		_, unspent := dataSecret("cs_check_data_unspent")
		checked := check(unspent)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusOk)
		connect.AssertEqual(t, checked.Kind, model.BalanceCodeKindData)

		// redeemed, or refunded: unknown, with no kind
		_, redeemed := dataSecret("cs_check_data_redeemed")
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		redeemResult, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: redeemed, NetworkId: networkId}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, redeemResult.Error == nil, true)
		voidedId, voided := dataSecret("cs_check_data_voided")
		cryptoDbExec(ctx, `UPDATE transfer_balance_code SET cancel_time = $2 WHERE balance_code_id = $1`, voidedId, server.NowUtc())
		for _, secret := range []string{redeemed, voided} {
			checked := check(secret)
			connect.AssertEqual(t, *checked, GiftCheckCodeResult{Status: model.GiftCodeStatusNotFound})
		}

		// a gift code says it is redeemed or refunded
		order, sessionId := giftDeliveryTestPendingOrder(t, ctx, 2, model.GiftDeliveryMe, time.Time{})
		codes := giftDeliveryTestFulfil(t, ctx, order.GiftOrderId, sessionId, server.NowUtc())
		giftCryptoTestRedeem(t, ctx, codes[0].Secret)
		cryptoDbExec(ctx, `UPDATE transfer_balance_code SET cancel_time = $2 WHERE balance_code_id = $1`, codes[1].BalanceCodeId, server.NowUtc())
		checked = check(codes[0].Secret)
		connect.AssertEqual(t, checked.Kind, model.BalanceCodeKindProGift)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusRedeemed)
		checked = check(codes[1].Secret)
		connect.AssertEqual(t, checked.Kind, model.BalanceCodeKindProGift)
		connect.AssertEqual(t, checked.Status, model.GiftCodeStatusVoided)
	})
}
