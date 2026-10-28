package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Pure tests: no database, redis or Slack. Each payment gate runs as written
// against salesLedgerTx, and the notices it registers on its transaction are
// captured where a server-owned transaction would queue them at commit
// (model.Testing_CaptureSalesPaymentNotices). Owner request 2026-10-28.

func captureSalesNotices(t *testing.T) func() []*model.SalesPaymentNotice {
	notices, restore := model.Testing_CaptureSalesPaymentNotices()
	t.Cleanup(restore)
	return notices
}

// salesNoticeLines is the posted text of a notice for a network named name.
func salesNoticeLines(notice *model.SalesPaymentNotice, networkName string) string {
	return model.SalesPaymentNoticeText(notice, networkName)
}

func salesNoticeText(item string, network string, amount string, method string) string {
	return fmt.Sprintf("*Sale:* %s\n*Network:* %s\n*Amount:* %s\n*Payment method:* %s", item, network, amount, method)
}

// ----- stripe -----

// A redelivered invoice.paid (or the reconciler crediting an invoice the
// webhook already credited) finds the stripe_invoice row and posts nothing; a
// new invoice of the same subscription is a renewal and posts once.
func TestStripeInvoiceCreditPostsOncePerInvoice(t *testing.T) {
	notices := captureSalesNotices(t)
	ctx := context.Background()
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	monthEnd := start.Add(31*24*time.Hour + manualPaymentGracePeriod)
	newInvoice := &StripeEventInvoiceObject{Id: "in_new", Total: 499, Currency: "usd", BillingReason: "subscription_create"}

	for range 3 {
		credited, err := stripeCreditInvoicePaidInTx(tx, ctx, networkId, newInvoice, model.UsdToNanoCents(3.49), start, monthEnd)
		connect.AssertEqual(t, err, nil)
		_ = credited
	}
	connect.AssertEqual(t, len(notices()), 1)

	renewalStart := start.Add(365 * 24 * time.Hour)
	yearEnd := renewalStart.Add(365*24*time.Hour + manualPaymentGracePeriod)
	renewal := &StripeEventInvoiceObject{Id: "in_renewal", Total: 3999, Currency: "usd", BillingReason: "subscription_cycle"}
	credited, err := stripeCreditInvoicePaidInTx(tx, ctx, networkId, renewal, model.UsdToNanoCents(27.99), renewalStart, yearEnd)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, credited, true)
	credited, err = stripeCreditInvoicePaidInTx(tx, ctx, networkId, renewal, model.UsdToNanoCents(27.99), renewalStart, yearEnd)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, credited, false)

	got := notices()
	connect.AssertEqual(t, len(got), 2)
	connect.AssertEqual(t, got[0].RecordKey, "stripe_invoice/in_new")
	connect.AssertEqual(t, *got[0].NetworkId, networkId)
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), salesNoticeText("Pro monthly (new)", "acme", "4.99 USD", "Stripe"))
	connect.AssertEqual(t, got[1].RecordKey, "stripe_invoice/in_renewal")
	connect.AssertEqual(t, salesNoticeLines(got[1], "acme"), salesNoticeText("Pro yearly (renewal)", "acme", "39.99 USD", "Stripe"))
}

// A network that is gone credits nothing and posts nothing.
func TestStripeInvoiceCreditForAMissingNetworkPostsNothing(t *testing.T) {
	notices := captureSalesNotices(t)
	tx := newSalesLedgerTx()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	credited, err := stripeCreditInvoicePaidInTx(tx, context.Background(), server.NewId(), &StripeEventInvoiceObject{Id: "in_x", Total: 499, Currency: "usd"}, 0, start, start.Add(61*24*time.Hour))
	connect.AssertEqual(t, err, model.ErrPaymentNetworkNotFound)
	connect.AssertEqual(t, credited, false)
	connect.AssertEqual(t, len(notices()), 0)
}

// stripeSalesApi stands up a fake Stripe API for the notice's read back.
func stripeSalesApi(t *testing.T, objects map[string]string) *sync.Map {
	requested := &sync.Map{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.Path, r.Header.Get("Authorization"))
		body, ok := objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(api.Close)

	previousBaseUrl := stripeApiBaseUrl
	previousToken := stripeApiTokenFunc
	stripeApiBaseUrl = api.URL
	stripeApiTokenFunc = func() string { return "sk_test_sales" }
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousToken
	})
	return requested
}

// The worker reads the invoice back from Stripe: the amount paid, new or
// renewal, the plan's interval and the charge's payment method, card or Link.
func TestStripeInvoiceSalesNoticeReadsThePaymentFromStripe(t *testing.T) {
	stripeSalesApi(t, map[string]string{
		"/v1/invoices/in_card":          `{"id":"in_card","billing_reason":"subscription_create","amount_paid":499,"total":499,"currency":"usd","charge":"ch_card","lines":{"data":[{"price":{"recurring":{"interval":"month"}}}]}}`,
		"/v1/charges/ch_card":           `{"id":"ch_card","payment_method_details":{"type":"card","card":{"brand":"visa","wallet":null}}}`,
		"/v1/invoices/in_link":          `{"id":"in_link","billing_reason":"subscription_cycle","amount_paid":3999,"total":3999,"currency":"usd","charge":"ch_link","lines":{"data":[{"plan":{"interval":"year"}}]}}`,
		"/v1/charges/ch_link":           `{"id":"ch_link","payment_method_details":{"type":"link"}}`,
		"/v1/invoices/in_wallet":        `{"id":"in_wallet","billing_reason":"subscription_cycle","amount_paid":499,"currency":"eur","charge":null,"payment_intent":"pi_wallet"}`,
		"/v1/payment_intents/pi_wallet": `{"id":"pi_wallet","latest_charge":"ch_wallet"}`,
		"/v1/charges/ch_wallet":         `{"id":"ch_wallet","payment_method_details":{"type":"card","card":{"wallet":{"type":"link"}}}}`,
		"/v1/invoices/in_trial":         `{"id":"in_trial","billing_reason":"subscription_create","amount_paid":0,"total":0,"currency":"usd","charge":null,"payment_intent":null}`,
		"/v1/invoices/in_yen":           `{"id":"in_yen","billing_reason":"subscription_cycle","amount_paid":980,"currency":"jpy","charge":"ch_yen"}`,
		"/v1/charges/ch_yen":            `{"id":"ch_yen","payment_method_details":{"type":"card","card":{"wallet":{"type":"apple_pay"}}}}`,
	})

	networkId := server.NewId()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		invoice *StripeEventInvoiceObject
		end     time.Time
		want    string
	}{
		{
			// as delivered, without the reason: the read back says new
			invoice: &StripeEventInvoiceObject{Id: "in_card", Total: 499, Currency: "usd"},
			end:     start.Add(31*24*time.Hour + manualPaymentGracePeriod),
			want:    salesNoticeText("Pro monthly (new)", "acme", "4.99 USD", "Stripe (card)"),
		},
		{
			// the held trial invoice path delivers no currency
			invoice: &StripeEventInvoiceObject{Id: "in_link", Total: 3999},
			end:     start.Add(14 * 24 * time.Hour),
			want:    salesNoticeText("Pro yearly (renewal)", "acme", "39.99 USD", "Stripe (Link)"),
		},
		{
			invoice: &StripeEventInvoiceObject{Id: "in_wallet"},
			end:     start.Add(31*24*time.Hour + manualPaymentGracePeriod),
			want:    salesNoticeText("Pro monthly (renewal)", "acme", "4.99 EUR", "Stripe (Link)"),
		},
		{
			invoice: &StripeEventInvoiceObject{Id: "in_trial", BillingReason: "subscription_create"},
			end:     start.Add(14 * 24 * time.Hour),
			want:    salesNoticeText("Pro (new, no charge)", "acme", "0.00 USD", "Stripe (no charge)"),
		},
		{
			invoice: &StripeEventInvoiceObject{Id: "in_yen"},
			end:     start.Add(365*24*time.Hour + manualPaymentGracePeriod),
			want:    salesNoticeText("Pro yearly (renewal)", "acme", "980 JPY", "Stripe (card, Apple Pay)"),
		},
	}
	for _, c := range cases {
		notice := stripeInvoiceSalesNotice(networkId, c.invoice, start, c.end)
		notice.Enrich(context.Background(), notice)
		connect.AssertEqual(t, salesNoticeLines(notice, "acme"), c.want)
	}
}

// When Stripe cannot be read, the notice keeps what the delivered invoice
// said rather than guess.
func TestStripeInvoiceSalesNoticeWithoutStripeKeepsTheDeliveredInvoice(t *testing.T) {
	stripeSalesApi(t, map[string]string{})
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	notice := stripeInvoiceSalesNotice(server.NewId(), &StripeEventInvoiceObject{Id: "in_gone", Total: 499, Currency: "usd", BillingReason: "subscription_cycle"}, start, start.Add(30*24*time.Hour+manualPaymentGracePeriod))
	notice.Enrich(context.Background(), notice)
	connect.AssertEqual(t, salesNoticeLines(notice, "acme"), salesNoticeText("Pro monthly (renewal)", "acme", "4.99 USD", "Stripe"))

	// and without a currency the amount is not reported
	notice = stripeInvoiceSalesNotice(server.NewId(), &StripeEventInvoiceObject{Id: "in_gone", Total: 499}, start, start.Add(30*24*time.Hour+manualPaymentGracePeriod))
	notice.Enrich(context.Background(), notice)
	connect.AssertEqual(t, notice.Amount, "not reported by Stripe")
}

// A data line of a checkout session: its amount, and the payment method of
// the session's payment intent.
func TestStripeCheckoutSalesNotice(t *testing.T) {
	requested := stripeSalesApi(t, map[string]string{
		"/v1/payment_intents/pi_data": `{"id":"pi_data","latest_charge":"ch_data"}`,
		"/v1/charges/ch_data":         `{"id":"ch_data","payment_method_details":{"type":"link"}}`,
		"/v1/payment_intents/pi_old":  `{"id":"pi_old","charges":{"data":[{"payment_method_details":{"type":"card"}}]}}`,
	})
	networkId := server.NewId()
	line := &StripeLineItem{AmountTotal: 20000, Currency: "usd", Quantity: 2}

	notice := stripeCheckoutSalesNotice("cs_1/1", 2*model.Tib, line, "pi_data", &networkId)
	connect.AssertEqual(t, notice.RecordKey, "balance_code/cs_1/1")
	notice.Enrich(context.Background(), notice)
	connect.AssertEqual(t, salesNoticeLines(notice, "acme"), salesNoticeText("2 TiB data (one-time)", "acme", "200.00 USD", "Stripe (Link)"))
	authorization, _ := requested.Load("/v1/payment_intents/pi_data")
	connect.AssertEqual(t, authorization, "Bearer sk_test_sales")

	notice = stripeCheckoutSalesNotice("cs_2", model.Tib, &StripeLineItem{AmountTotal: 10000, Currency: "usd"}, "pi_old", nil)
	notice.Enrich(context.Background(), notice)
	connect.AssertEqual(t, salesNoticeLines(notice, ""), salesNoticeText("1 TiB data (one-time)", "none yet (a data code, redeemed later)", "100.00 USD", "Stripe (card)"))

	// no payment intent: nothing to read
	notice = stripeCheckoutSalesNotice("cs_3", model.Tib, &StripeLineItem{AmountTotal: 10000, Currency: "usd"}, "", &networkId)
	connect.AssertEqual(t, notice.Enrich == nil, true)
	connect.AssertEqual(t, notice.Method, "Stripe")
}

func TestStripeSalesPaymentMethod(t *testing.T) {
	cases := map[string]string{
		`{}`: "Stripe",
		`{"payment_method_details":{"type":"card"}}`:                                         "Stripe (card)",
		`{"payment_method_details":{"type":"card","card":{"wallet":{"type":"link"}}}}`:       "Stripe (Link)",
		`{"payment_method_details":{"type":"link"}}`:                                         "Stripe (Link)",
		`{"payment_method_details":{"type":"card","card":{"wallet":{"type":"google_pay"}}}}`: "Stripe (card, Google Pay)",
		`{"payment_method_details":{"type":"cashapp"}}`:                                      "Stripe (Cash App Pay)",
		`{"payment_method_details":{"type":"us_bank_account"}}`:                              "Stripe (US bank account)",
		`{"payment_method_details":{"type":"new_method_2027"}}`:                              "Stripe (new method 2027)",
		`{"payment_method_details":{"type":"<!channel>"}}`:                                   "Stripe (other)",
	}
	for body, want := range cases {
		var charge stripeSalesCharge
		connect.AssertEqual(t, json.Unmarshal([]byte(body), &charge), nil)
		connect.AssertEqual(t, stripeSalesPaymentMethod(&charge), want)
	}
}

// ----- coinbase -----

func TestCoinbaseSalesNotice(t *testing.T) {
	networkId := server.NewId()
	payment := &CoinbaseEventDataPayment{Net: &CoinbaseEventDataPaymentNet{
		Local:  &CoinbaseEventDataPaymentAmount{Amount: "95.00", Currency: "USD"},
		Crypto: &CoinbaseEventDataPaymentAmount{Amount: "95.000000", Currency: "USDC"},
	}}
	notice := coinbaseSalesNotice("charge-1", model.Tib, payment, &networkId)
	connect.AssertEqual(t, notice.RecordKey, "balance_code/charge-1")
	connect.AssertEqual(t, salesNoticeLines(notice, "acme"), salesNoticeText("1 TiB data (one-time)", "acme", "95.00 USD (net of the Coinbase fee)", "Coinbase Commerce (USDC)"))

	// a malformed amount is not reported rather than guessed
	payment.Net.Local.Amount = "95 dollars"
	notice = coinbaseSalesNotice("charge-2", model.Tib, payment, nil)
	connect.AssertEqual(t, notice.Amount, "not reported by Coinbase")
}

// ----- apple -----

func appleSalesTransaction(networkId server.Id, transactionId string) *validatedAppleTransaction {
	purchase := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return &validatedAppleTransaction{
		networkId:             networkId,
		transactionId:         transactionId,
		originalTransactionId: "1000",
		productId:             "supporter_monthly_26",
		purchaseTime:          purchase,
		expiresTime:           purchase.Add(31 * 24 * time.Hour),
		netRevenue:            model.UsdToNanoCents(3.99),
		priceMilliunits:       4990,
		currency:              "USD",
	}
}

// The notification, a client's verify report, an offer-code binding and the
// reconciler all credit an App Store transaction through the
// apple_subscription_transaction ledger: whichever comes first posts, and the
// rest post nothing.
func TestAppleCreditPostsOncePerTransaction(t *testing.T) {
	notices := captureSalesNotices(t)
	ctx := context.Background()
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)

	first := appleSalesTransaction(networkId, "1000")
	first.transactionReason = "PURCHASE"
	for range 3 {
		appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), first)
	}
	connect.AssertEqual(t, len(notices()), 1)

	renewal := appleSalesTransaction(networkId, "1001")
	renewal.transactionReason = "RENEWAL"
	renewal.productId = "supporter_yearly_26"
	renewal.priceMilliunits = 39990
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), renewal), true)
	connect.AssertEqual(t, appleCreditSubscriptionTransactionInTx(tx, ctx, server.NewId(), renewal), false)

	got := notices()
	connect.AssertEqual(t, len(got), 2)
	connect.AssertEqual(t, got[0].RecordKey, "apple_transaction/1000")
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), salesNoticeText("Pro monthly (new)", "acme", "4.99 USD", "Apple App Store"))
	connect.AssertEqual(t, salesNoticeLines(got[1], "acme"), salesNoticeText("Pro yearly (renewal)", "acme", "39.99 USD", "Apple App Store"))
}

func TestAppleSalesNotice(t *testing.T) {
	networkId := server.NewId()

	// no transactionReason: the first transaction of the subscription is new,
	// a later one a renewal
	transaction := appleSalesTransaction(networkId, "1000")
	connect.AssertEqual(t, appleSalesNotice(transaction).Kind, "new")
	transaction = appleSalesTransaction(networkId, "1007")
	connect.AssertEqual(t, appleSalesNotice(transaction).Kind, "renewal")

	// the legacy monthly product, in yen
	transaction = appleSalesTransaction(networkId, "1008")
	transaction.productId = "supporter"
	transaction.priceMilliunits = 600_000
	transaction.currency = "JPY"
	transaction.transactionReason = "RENEWAL"
	connect.AssertEqual(t, salesNoticeLines(appleSalesNotice(transaction), "acme"), salesNoticeText("Pro monthly (renewal)", "acme", "600 JPY", "Apple App Store"))

	// a free trial
	transaction = appleSalesTransaction(networkId, "1000")
	transaction.transactionReason = "PURCHASE"
	transaction.priceMilliunits = 0
	transaction.offerDiscountType = "FREE_TRIAL"
	connect.AssertEqual(t, salesNoticeLines(appleSalesNotice(transaction), "acme"), salesNoticeText("Pro monthly (new, free trial)", "acme", "0.00 USD", "Apple App Store"))

	// a price without a currency says so
	transaction = appleSalesTransaction(networkId, "1009")
	transaction.currency = ""
	connect.AssertEqual(t, appleSalesNotice(transaction).Amount, "4.99 (currency not reported)")
}

// ----- google play -----

func playSalesSubscription(orderId string, expiry time.Time, recurringPrice string) *PlaySubscription {
	item := &PlaySubscriptionPurchaseLineItem{
		ProductId:               "supporter_monthly",
		ExpiryTime:              expiry.Format(time.RFC3339),
		LatestSuccessfulOrderId: orderId,
	}
	if recurringPrice != "" {
		item.AutoRenewingPlan = &PlayAutoRenewingPlan{AutoRenewEnabled: true, RecurringPrice: json.RawMessage(recurringPrice)}
	}
	return &PlaySubscription{
		LineItems:         []*PlaySubscriptionPurchaseLineItem{item},
		StartTime:         "2026-10-01T00:00:00Z",
		SubscriptionState: "SUBSCRIPTION_STATE_ACTIVE",
	}
}

// The RTDN, the scheduled renewal poll, a client's verify report and the
// reconciler all credit a Play window through the purchase-token lock and the
// overlap re-check: the window posts once, and the next window (a renewal)
// posts once more.
func TestPlayCreditPostsOncePerWindow(t *testing.T) {
	notices := captureSalesNotices(t)
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	sku := &Sku{Supporter: true, PriceAmountUsd: 4.99, FeeFraction: 0.15}
	args := &PlaySubscriptionRenewalArgs{
		NetworkId:      networkId,
		PackageName:    "network.ur.test",
		SubscriptionId: "supporter_monthly",
		PurchaseToken:  "token-1",
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// the window's expiry is in the future: the credit refuses a window that
	// already ended
	firstExpiry := server.NowUtc().Add(30 * 24 * time.Hour).Truncate(time.Second)
	first := playSalesSubscription("GPA.1234-5678-9012-34567", firstExpiry, `{"currencyCode":"EUR","units":"4","nanos":990000000}`)

	for range 3 {
		_, err := playCreditSubscriptionInTx(tx, clientSession, args, first, sku, start, firstExpiry)
		connect.AssertEqual(t, err, nil)
	}
	connect.AssertEqual(t, len(notices()), 1)

	secondExpiry := firstExpiry.Add(30 * 24 * time.Hour)
	second := playSalesSubscription("GPA.1234-5678-9012-34567..0", secondExpiry, "")
	renewed, err := playCreditSubscriptionInTx(tx, clientSession, args, second, sku, start, secondExpiry)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, renewed, true)
	renewed, err = playCreditSubscriptionInTx(tx, clientSession, args, second, sku, start, secondExpiry)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, renewed, false)

	got := notices()
	connect.AssertEqual(t, len(got), 2)
	connect.AssertEqual(t, got[0].RecordKey, fmt.Sprintf("play_purchase/token-1/%d", firstExpiry.Unix()))
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), salesNoticeText("Pro monthly (new)", "acme", "4.99 EUR (Google Play list price, before tax and discounts)", "Google Play"))
	connect.AssertEqual(t, salesNoticeLines(got[1], "acme"), salesNoticeText("Pro monthly (renewal)", "acme", "4.99 USD (configured list price; Google Play does not report the charged amount)", "Google Play"))
}

func TestPlaySalesNoticeDataPack(t *testing.T) {
	networkId := server.NewId()
	sku := &Sku{BalanceByteCountHumanReadable: "100gib", PriceAmountUsd: 9.99}
	notice := playSalesNotice(&PlaySubscriptionRenewalArgs{NetworkId: networkId, SubscriptionId: "data_100gib", PurchaseToken: "token-2"}, &PlaySubscription{LatestOrderId: "GPA.1"}, sku, time.Unix(1_800_000_000, 0))
	connect.AssertEqual(t, salesNoticeLines(notice, "acme"), salesNoticeText("100 GiB data (new)", "acme", "9.99 USD (configured list price; Google Play does not report the charged amount)", "Google Play"))
}

// ----- x402 -----

// An agent retrying a settled payment, on this or another api instance, or
// the reconciler retrying the grant, finds the grant and posts nothing.
func TestX402GrantPostsOncePerSettleTransaction(t *testing.T) {
	notices := captureSalesNotices(t)
	ctx := context.Background()
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	pro := &X402Sku{SkuId: X402SkuProMonth, PriceUsd: 4.99, Pro: true, ByteCount: 600 * model.Gib}
	settle := &X402SettleResponse{Success: true, Transaction: "0xpro", Network: X402NetworkBase}
	for range 3 {
		_, err := x402GrantProMonthInTx(tx, ctx, networkId, pro, model.UsdToNanoCents(4.99), settle, nil, start, start.Add(60*24*time.Hour))
		connect.AssertEqual(t, err, nil)
	}

	data := &X402Sku{SkuId: X402SkuData1Tib, PriceUsd: 95, ByteCount: model.Tib}
	dataSettle := &X402SettleResponse{Success: true, Transaction: "5xSolanaSignature", Network: X402NetworkSolana}
	for range 2 {
		_, err := x402GrantDataInTx(tx, ctx, networkId, data, model.UsdToNanoCents(95), dataSettle, nil, start, start.Add(365*24*time.Hour))
		connect.AssertEqual(t, err, nil)
	}

	// each grant checks for an earlier grant only under the settle
	// transaction's lock, so concurrent grants of one payment serialize
	connect.AssertEqual(t, tx.log, []string{
		"advisory_lock x402_settle/0xpro", "x402_granted 0xpro",
		"advisory_lock x402_settle/0xpro", "x402_granted 0xpro",
		"advisory_lock x402_settle/0xpro", "x402_granted 0xpro",
		"advisory_lock x402_settle/5xSolanaSignature", "x402_granted 5xSolanaSignature",
		"advisory_lock x402_settle/5xSolanaSignature", "x402_granted 5xSolanaSignature",
	})

	got := notices()
	connect.AssertEqual(t, len(got), 2)
	connect.AssertEqual(t, got[0].RecordKey, "x402/0xpro")
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), salesNoticeText("Pro monthly (one-time)", "acme", "4.99 USDC", "x402 USDC on Base"))
	connect.AssertEqual(t, salesNoticeLines(got[1], "acme"), salesNoticeText("1 TiB data (one-time)", "acme", "95.00 USDC", "x402 USDC on Solana"))
}

// ----- solana pay -----

// A Helius redelivery, or the reconciler sweeping a payment the webhook
// already credited, finds the intent consumed and posts nothing.
func TestSolanaCreditPostsOncePerIntent(t *testing.T) {
	notices := captureSalesNotices(t)
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	yearly := &model.PaymentIntentSearchResult{NetworkId: &networkId, PaymentReference: "ref-yearly", ExpectedAmountUsd: 40.000123, SubscriptionPlan: model.SolanaPlanYearly}
	for range 3 {
		_, err := solanaCreditPlanIntentInTx(tx, clientSession, yearly, "sig-1", 40.000123, start, start.Add(395*24*time.Hour))
		connect.AssertEqual(t, err, nil)
	}

	data := &model.PaymentIntentSearchResult{NetworkId: &networkId, PaymentReference: "ref-data", ExpectedAmountUsd: 95.000017, SubscriptionPlan: StripeItemData1Tib}
	for range 2 {
		_, err := solanaCreditDataPackInTx(tx, clientSession, data, "sig-2", 95.000017, networkId, model.Tib, start, 365*24*time.Hour, model.UsdToNanoCents(95))
		connect.AssertEqual(t, err, nil)
	}

	got := notices()
	connect.AssertEqual(t, len(got), 2)
	connect.AssertEqual(t, got[0].RecordKey, "solana_intent/ref-yearly")
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), salesNoticeText("Pro yearly (one-time)", "acme", "40.000123 USDC", "USDC on Solana (Solana Pay)"))
	connect.AssertEqual(t, salesNoticeLines(got[1], "acme"), salesNoticeText("1 TiB data (one-time)", "acme", "95.000017 USDC", "USDC on Solana (Solana Pay)"))
}

// ----- crypto invoices -----

// The scanner, the reconciler and an operator credit an invoice through its
// one-shot: the first posts, the rest post nothing.
func TestCryptoCreditPostsOncePerInvoice(t *testing.T) {
	notices := captureSalesNotices(t)
	networkId := server.NewId()
	tx := newSalesLedgerTx(networkId)
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	yearly := &model.CryptoPaymentIntent{
		IntentId:          server.NewId(),
		Asset:             model.CryptoAssetXmr,
		Delivery:          model.CryptoDeliveryAccount,
		NetworkId:         &networkId,
		Item:              model.SolanaPlanYearly,
		PriceUsdNanoCents: model.UsdToNanoCents(40),
		AmountAtomic:      123_456_789_000,
	}
	credit := &cryptoCredit{usdNanoCents: model.UsdToNanoCents(40)}
	for range 3 {
		_, err := cryptoCreditIntentInTx(tx, clientSession, yearly, credit, true, 0, 365*24*time.Hour, now)
		connect.AssertEqual(t, err, nil)
	}

	code := &model.CryptoPaymentIntent{
		IntentId:          server.NewId(),
		Asset:             model.CryptoAssetZec,
		Delivery:          model.CryptoDeliveryCode,
		Item:              StripeItemData1Tib,
		PriceUsdNanoCents: model.UsdToNanoCents(95),
		AmountAtomic:      250_000_000,
	}
	lateCredit := &cryptoCredit{usdNanoCents: model.UsdToNanoCents(94.5), late: true}
	for range 2 {
		_, err := cryptoCreditIntentInTx(tx, clientSession, code, lateCredit, false, model.Tib, 365*24*time.Hour, now)
		connect.AssertEqual(t, err, nil)
	}

	got := notices()
	connect.AssertEqual(t, len(got), 2)
	connect.AssertEqual(t, got[0].RecordKey, "crypto_intent/"+yearly.IntentId.String())
	connect.AssertEqual(t, salesNoticeLines(got[0], "acme"), salesNoticeText("Pro yearly (one-time)", "acme", "0.123456789 XMR invoice (credited as 40.00 USD)", "Monero"))
	connect.AssertEqual(t, got[1].NetworkId == nil, true)
	connect.AssertEqual(t, salesNoticeLines(got[1], ""), salesNoticeText("1 TiB data (one-time)", "none yet (a data code, redeemed later)", "2.5 ZEC invoice (credited as 94.50 USD), paid late", "Zcash (shielded)"))
}

// ----- the purchase never depends on the notice -----

// A notice that cannot be built is counted and logged; the purchase's
// transaction goes on.
func TestSalesNoticeBuildFailureNeverFailsThePurchase(t *testing.T) {
	notices := captureSalesNotices(t)
	addSalesPaymentNoticeInTx(nil, func() *model.SalesPaymentNotice {
		var sku *Sku
		// a nil sku: the builder panics
		return playSalesNotice(&PlaySubscriptionRenewalArgs{}, &PlaySubscription{}, sku, time.Time{})
	})
	connect.AssertEqual(t, len(notices()), 0)
}
