package controller

// The sales channel notice of each payment path (owner request 2026-10-28;
// model/sales_payment_notice.go). Each payment path registers its notice in
// the transaction that newly creates the payment's durable record, on the
// branch where that record's idempotency gate inserted a row:
//
//	Stripe invoice (Pro, new and renewal)   stripe_invoice ledger insert
//	Stripe checkout and Coinbase (data)     transfer_balance_code insert
//	Apple App Store (Pro, new and renewal)  apple_subscription_transaction ledger insert
//	Google Play (Pro and data)              purchase-token lock and overlap re-check
//	x402 (Pro and data)                     settle-transaction lock and grant re-check
//	Solana Pay (Pro and data)               solana_payment_intent one-shot
//	crypto invoices (Monero, Zcash, USDC)   crypto_payment_intent one-shot
//
// The webhooks, the client reports, the scheduled renewals and the payment
// reconciler all credit through these gates, so each payment is posted once
// whichever of them records it. Building a notice never fails the purchase:
// a builder that panics is counted and logged, and the purchase goes on.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// addSalesPaymentNoticeInTx registers the notice build returns on tx's commit.
// It never fails the caller: the builders read only what the path already
// holds, and a builder that panics anyway is counted and logged.
func addSalesPaymentNoticeInTx(tx server.PgTx, build func() *model.SalesPaymentNotice) {
	var notice *model.SalesPaymentNotice
	if r := server.HandleError(func() {
		notice = build()
	}); r != nil {
		model.CountSalesPaymentNotice(model.SalesPaymentNoticeResultBuildError)
		glog.Errorf("[sales]payment notice could not be built: %v\n", r)
		return
	}
	model.AddSalesPaymentNoticeInTx(tx, notice)
}

// salesPlanForProductId is the plan a store product id sells. "supporter" is
// the App Store's monthly plan from 2025-02 (appleLegacySupporterProductIds).
func salesPlanForProductId(productId string) string {
	if strings.EqualFold(productId, "supporter") {
		return model.PlanMonthly
	}
	return planForProductId(productId)
}

// salesConfiguredUsdAmount is a configured USD price, marked as the list
// price.
func salesConfiguredUsdAmount(priceUsd float64, store string) string {
	if !(0 < priceUsd) {
		return fmt.Sprintf("not reported by %s", store)
	}
	return fmt.Sprintf(
		"%s (configured list price; %s does not report the charged amount)",
		model.SalesAmount64(int64(math.Round(priceUsd*100)), 2, "USD"),
		store,
	)
}

// salesNanoCentsUsd is a USD amount held as model.NanoCents, exactly
// (model.UsdToNanoCents: a dollar is 1e9).
func salesNanoCentsUsd(nanoCents model.NanoCents) string {
	return model.SalesAmount64(nanoCents, 9, "USD")
}

// ----- stripe -----

// stripeCurrencyExponent is the decimals of a Stripe amount in currency:
// Stripe's zero-decimal and three-decimal currencies, two otherwise
// (docs.stripe.com/currencies).
func stripeCurrencyExponent(currency string) int {
	switch strings.ToUpper(currency) {
	case "BIF", "CLP", "DJF", "GNF", "JPY", "KMF", "KRW", "MGA", "PYG", "RWF",
		"UGX", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	case "BHD", "JOD", "KWD", "OMR", "TND":
		return 3
	default:
		return 2
	}
}

// stripeSalesAmount is a Stripe amount in its currency's smallest unit.
func stripeSalesAmount(amount int64, currency string) string {
	currency = model.SalesCurrencyCode(currency)
	if currency == "" {
		return "not reported by Stripe"
	}
	return model.SalesAmount64(amount, stripeCurrencyExponent(currency), currency)
}

// stripeSalesKind is new, renewal or plan change from an invoice's
// billing_reason.
func stripeSalesKind(billingReason string, zeroAmount bool) string {
	switch billingReason {
	case "subscription_create":
		if zeroAmount {
			return "new, no charge"
		}
		return model.SalesPaymentKindNew
	case "subscription_cycle":
		return model.SalesPaymentKindRenewal
	case "subscription_update":
		return model.SalesPaymentKindPlanChange
	}
	return ""
}

// stripeSalesPlanForWindow is the plan of a paid window that ends
// manualPaymentGracePeriod after the paid period (stripeResolveInvoiceCredit).
func stripeSalesPlanForWindow(startTime time.Time, endTime time.Time) string {
	paid := endTime.Sub(startTime) - manualPaymentGracePeriod
	day := 24 * time.Hour
	switch {
	case 27*day <= paid && paid <= 32*day:
		return model.PlanMonthly
	case 360*day <= paid && paid <= 370*day:
		return model.PlanYearly
	}
	return ""
}

// stripeSalesPlanForInterval is the plan of a recurring price's interval.
func stripeSalesPlanForInterval(interval string) string {
	switch interval {
	case "month":
		return model.PlanMonthly
	case "year":
		return model.PlanYearly
	}
	return ""
}

// stripeInvoiceSalesNotice is the notice of a credited subscription invoice.
// What the delivered invoice carries is the fallback; the worker reads the
// invoice back from Stripe for the amount paid, the reason, the plan and the
// charge's payment method.
func stripeInvoiceSalesNotice(
	networkId server.Id,
	invoice *StripeEventInvoiceObject,
	startTime time.Time,
	endTime time.Time,
) *model.SalesPaymentNotice {
	notice := &model.SalesPaymentNotice{
		RecordKey: "stripe_invoice/" + invoice.Id,
		NetworkId: &networkId,
		Item:      model.SalesProItem(stripeSalesPlanForWindow(startTime, endTime)),
		Kind:      stripeSalesKind(invoice.BillingReason, invoice.Total == 0),
		Amount:    "not reported by Stripe",
		Method:    "Stripe",
	}
	if model.SalesCurrencyCode(invoice.Currency) != "" {
		notice.Amount = stripeSalesAmount(int64(invoice.Total), invoice.Currency)
	}
	invoiceId := invoice.Id
	notice.Enrich = func(ctx context.Context, notice *model.SalesPaymentNotice) {
		stripeEnrichInvoiceSalesNotice(ctx, invoiceId, notice)
	}
	return notice
}

// stripeSalesInvoice is what a notice reads off a Stripe invoice. Every field
// is optional, since the invoice's shape differs across API versions.
type stripeSalesInvoice struct {
	BillingReason string          `json:"billing_reason"`
	AmountPaid    *int64          `json:"amount_paid"`
	Total         *int64          `json:"total"`
	Currency      string          `json:"currency"`
	Charge        json.RawMessage `json:"charge"`
	PaymentIntent json.RawMessage `json:"payment_intent"`
	Lines         *struct {
		Data []*struct {
			Price json.RawMessage `json:"price"`
			Plan  json.RawMessage `json:"plan"`
		} `json:"data"`
	} `json:"lines"`
}

// interval is the first recurring interval of the invoice's lines.
func (self *stripeSalesInvoice) interval() string {
	if self.Lines == nil {
		return ""
	}
	for _, line := range self.Lines.Data {
		if line == nil {
			continue
		}
		var price struct {
			Recurring *struct {
				Interval string `json:"interval"`
			} `json:"recurring"`
		}
		if json.Unmarshal(line.Price, &price) == nil && price.Recurring != nil && price.Recurring.Interval != "" {
			return price.Recurring.Interval
		}
		var plan struct {
			Interval string `json:"interval"`
		}
		if json.Unmarshal(line.Plan, &plan) == nil && plan.Interval != "" {
			return plan.Interval
		}
	}
	return ""
}

// stripeSalesCharge is the payment method a charge reports.
type stripeSalesCharge struct {
	PaymentMethodDetails *struct {
		Type string `json:"type"`
		Card *struct {
			Wallet *struct {
				Type string `json:"type"`
			} `json:"wallet"`
		} `json:"card"`
	} `json:"payment_method_details"`
}

// stripeSalesPaymentIntent is where a payment intent names its charge: the id
// of its latest charge, or (older API versions) the charges themselves.
type stripeSalesPaymentIntent struct {
	LatestCharge json.RawMessage `json:"latest_charge"`
	Charges      *struct {
		Data []*stripeSalesCharge `json:"data"`
	} `json:"charges"`
}

var stripeSalesPaymentMethodTypePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

var stripeSalesPaymentMethodNames = map[string]string{
	"acss_debit":        "pre-authorized debit",
	"affirm":            "Affirm",
	"afterpay_clearpay": "Afterpay",
	"alipay":            "Alipay",
	"amazon_pay":        "Amazon Pay",
	"apple_pay":         "Apple Pay",
	"bancontact":        "Bancontact",
	"cashapp":           "Cash App Pay",
	"crypto":            "crypto",
	"google_pay":        "Google Pay",
	"ideal":             "iDEAL",
	"klarna":            "Klarna",
	"link":              "Link",
	"paypal":            "PayPal",
	"revolut_pay":       "Revolut Pay",
	"samsung_pay":       "Samsung Pay",
	"sepa_debit":        "SEPA Direct Debit",
	"us_bank_account":   "US bank account",
	"wechat_pay":        "WeChat Pay",
}

func stripeSalesPaymentMethodName(paymentMethodType string) string {
	if name, ok := stripeSalesPaymentMethodNames[paymentMethodType]; ok {
		return name
	}
	if stripeSalesPaymentMethodTypePattern.MatchString(paymentMethodType) {
		return strings.ReplaceAll(paymentMethodType, "_", " ")
	}
	return "other"
}

// stripeSalesPaymentMethod is "Stripe (card)", "Stripe (Link)", ... from the
// charge's payment method details: Link pays either as its own payment method
// type or as a card in the Link wallet.
func stripeSalesPaymentMethod(charge *stripeSalesCharge) string {
	if charge == nil || charge.PaymentMethodDetails == nil || charge.PaymentMethodDetails.Type == "" {
		return "Stripe"
	}
	details := charge.PaymentMethodDetails
	if details.Type == "card" {
		if details.Card != nil && details.Card.Wallet != nil && details.Card.Wallet.Type != "" {
			if details.Card.Wallet.Type == "link" {
				return "Stripe (Link)"
			}
			return fmt.Sprintf("Stripe (card, %s)", stripeSalesPaymentMethodName(details.Card.Wallet.Type))
		}
		return "Stripe (card)"
	}
	return fmt.Sprintf("Stripe (%s)", stripeSalesPaymentMethodName(details.Type))
}

func stripeSalesGet[R any](ctx context.Context, path string) (R, error) {
	return server.HttpGetRequireStatusOk[R](
		ctx,
		stripeApiBaseUrl+path,
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
		},
		server.ResponseJsonObject[R],
	)
}

// stripeSalesChargeMethod reads a charge's payment method.
func stripeSalesChargeMethod(ctx context.Context, chargeId string) (string, bool) {
	if chargeId == "" {
		return "", false
	}
	charge, err := stripeSalesGet[*stripeSalesCharge](ctx, "/v1/charges/"+url.PathEscape(chargeId))
	if err != nil || charge == nil {
		return "", false
	}
	return stripeSalesPaymentMethod(charge), true
}

// stripeSalesPaymentIntentMethod reads the payment method of a payment
// intent's charge.
func stripeSalesPaymentIntentMethod(ctx context.Context, paymentIntentId string) (string, bool) {
	if paymentIntentId == "" {
		return "", false
	}
	paymentIntent, err := stripeSalesGet[*stripeSalesPaymentIntent](ctx, "/v1/payment_intents/"+url.PathEscape(paymentIntentId))
	if err != nil || paymentIntent == nil {
		return "", false
	}
	if chargeId := stripeIdFromRaw(paymentIntent.LatestCharge); chargeId != "" {
		return stripeSalesChargeMethod(ctx, chargeId)
	}
	if paymentIntent.Charges != nil && 0 < len(paymentIntent.Charges.Data) && paymentIntent.Charges.Data[0] != nil {
		return stripeSalesPaymentMethod(paymentIntent.Charges.Data[0]), true
	}
	return "", false
}

// stripeEnrichInvoiceSalesNotice fills a notice in from the invoice as Stripe
// reports it now. Runs on the notice worker, after the commit.
func stripeEnrichInvoiceSalesNotice(ctx context.Context, invoiceId string, notice *model.SalesPaymentNotice) {
	invoice, err := stripeSalesGet[*stripeSalesInvoice](ctx, "/v1/invoices/"+url.PathEscape(invoiceId))
	if err != nil || invoice == nil {
		glog.Infof("[sales]invoice %s: could not read it back from stripe: %s\n", invoiceId, err)
		return
	}
	amount := invoice.AmountPaid
	if amount == nil {
		amount = invoice.Total
	}
	if amount != nil && model.SalesCurrencyCode(invoice.Currency) != "" {
		notice.Amount = stripeSalesAmount(*amount, invoice.Currency)
	}
	if kind := stripeSalesKind(invoice.BillingReason, amount != nil && *amount == 0); kind != "" {
		notice.Kind = kind
	}
	if plan := stripeSalesPlanForInterval(invoice.interval()); plan != "" {
		notice.Item = model.SalesProItem(plan)
	}
	if method, ok := stripeSalesChargeMethod(ctx, stripeIdFromRaw(invoice.Charge)); ok {
		notice.Method = method
	} else if method, ok := stripeSalesPaymentIntentMethod(ctx, stripeIdFromRaw(invoice.PaymentIntent)); ok {
		notice.Method = method
	} else if amount != nil && *amount == 0 {
		notice.Method = "Stripe (no charge)"
	}
}

// stripeCheckoutSalesNotice is the notice of one data line of a completed
// checkout session, made into a balance code.
func stripeCheckoutSalesNotice(
	purchaseEventId string,
	byteCount model.ByteCount,
	lineItem *StripeLineItem,
	paymentIntentId string,
	redeemNetworkId *server.Id,
) *model.SalesPaymentNotice {
	notice := &model.SalesPaymentNotice{
		RecordKey: "balance_code/" + purchaseEventId,
		NetworkId: redeemNetworkId,
		Item:      model.SalesDataItem(byteCount),
		Kind:      model.SalesPaymentKindOneTime,
		Amount:    stripeSalesAmount(int64(lineItem.AmountTotal), lineItem.Currency),
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

// ----- coinbase -----

var salesDecimalPattern = regexp.MustCompile(`^[0-9]{1,18}(\.[0-9]{1,18})?$`)

var coinbaseSalesCryptoPattern = regexp.MustCompile(`^[A-Z0-9]{2,10}$`)

// coinbaseSalesNotice is the notice of a confirmed Coinbase charge, made into
// a balance code. The webhook reads the payment's net amount, after Coinbase's
// fee, and the notice says so.
func coinbaseSalesNotice(
	chargeId string,
	byteCount model.ByteCount,
	payment *CoinbaseEventDataPayment,
	redeemNetworkId *server.Id,
) *model.SalesPaymentNotice {
	notice := &model.SalesPaymentNotice{
		RecordKey: "balance_code/" + chargeId,
		NetworkId: redeemNetworkId,
		Item:      model.SalesDataItem(byteCount),
		Kind:      model.SalesPaymentKindOneTime,
		Amount:    "not reported by Coinbase",
		Method:    "Coinbase Commerce",
	}
	if payment == nil || payment.Net == nil {
		return notice
	}
	if local := payment.Net.Local; local != nil && salesDecimalPattern.MatchString(local.Amount) {
		if currency := model.SalesCurrencyCode(local.Currency); currency != "" {
			notice.Amount = fmt.Sprintf("%s %s (net of the Coinbase fee)", local.Amount, currency)
		}
	}
	if crypto := payment.Net.Crypto; crypto != nil && coinbaseSalesCryptoPattern.MatchString(crypto.Currency) {
		notice.Method = fmt.Sprintf("Coinbase Commerce (%s)", crypto.Currency)
	}
	return notice
}

// ----- apple -----

// appleSalesKind is new or renewal: the transaction's reason (PURCHASE or
// RENEWAL), else whether it is the subscription's first transaction.
func appleSalesKind(transaction *validatedAppleTransaction) string {
	kind := ""
	switch transaction.transactionReason {
	case "RENEWAL":
		kind = model.SalesPaymentKindRenewal
	case "PURCHASE":
		kind = model.SalesPaymentKindNew
	default:
		if transaction.originalTransactionId != "" {
			if transaction.originalTransactionId == transaction.transactionId {
				kind = model.SalesPaymentKindNew
			} else {
				kind = model.SalesPaymentKindRenewal
			}
		}
	}
	if kind == model.SalesPaymentKindNew && transaction.priceMilliunits == 0 && transaction.offerDiscountType == "FREE_TRIAL" {
		kind = model.SalesPaymentKindFreeTrial
	}
	return kind
}

// appleSalesNotice is the notice of a credited App Store transaction. The
// transaction reports its price in milliunits of its currency.
func appleSalesNotice(transaction *validatedAppleTransaction) *model.SalesPaymentNotice {
	return &model.SalesPaymentNotice{
		RecordKey: "apple_transaction/" + transaction.transactionId,
		NetworkId: &transaction.networkId,
		Item:      model.SalesProItem(salesPlanForProductId(transaction.productId)),
		Kind:      appleSalesKind(transaction),
		Amount:    model.SalesAmount64(transaction.priceMilliunits, 3, transaction.currency),
		Method:    "Apple App Store",
	}
}

// ----- google play -----

// playSalesOrderId is the order that paid for the window being credited.
func playSalesOrderId(sub *PlaySubscription) string {
	var latest *PlaySubscriptionPurchaseLineItem
	for _, item := range sub.LineItems {
		if item == nil || item.LatestSuccessfulOrderId == "" {
			continue
		}
		if latest == nil || latest.ExpiryTime < item.ExpiryTime {
			latest = item
		}
	}
	if latest != nil {
		return latest.LatestSuccessfulOrderId
	}
	return sub.LatestOrderId
}

// playSalesKind is new or renewal from the order id: Play numbers each
// renewal's order after the first with a ".." suffix (GPA.1234-...-12345..0).
func playSalesKind(sub *PlaySubscription) string {
	orderId := playSalesOrderId(sub)
	switch {
	case orderId == "":
		return ""
	case strings.Contains(orderId, ".."):
		return model.SalesPaymentKindRenewal
	default:
		return model.SalesPaymentKindNew
	}
}

// playSalesRecurringPrice is a line item's recurring price as Play reports
// it (a Money: currency code, whole units and nano units), or "".
func playSalesRecurringPrice(item *PlaySubscriptionPurchaseLineItem) string {
	if item == nil || item.AutoRenewingPlan == nil || len(item.AutoRenewingPlan.RecurringPrice) == 0 {
		return ""
	}
	var money struct {
		CurrencyCode string          `json:"currencyCode"`
		Units        json.RawMessage `json:"units"`
		Nanos        int64           `json:"nanos"`
	}
	if json.Unmarshal(item.AutoRenewingPlan.RecurringPrice, &money) != nil {
		return ""
	}
	currency := model.SalesCurrencyCode(money.CurrencyCode)
	if currency == "" || money.Nanos < 0 || 1_000_000_000 <= money.Nanos {
		return ""
	}
	unitsText := strings.Trim(string(money.Units), `"`)
	if unitsText == "" {
		unitsText = "0"
	}
	units, err := strconv.ParseInt(unitsText, 10, 64)
	if err != nil || units < 0 {
		return ""
	}
	value := new(big.Int).Mul(big.NewInt(units), big.NewInt(1_000_000_000))
	value.Add(value, big.NewInt(money.Nanos))
	return model.SalesAmount(value, 9, currency)
}

// playSalesAmount is the purchase's recurring price as Google Play reports
// it, which leaves out tax and discounts; else the configured list price.
// Play does not report the charged amount here.
func playSalesAmount(sub *PlaySubscription, sku *Sku, productId string) string {
	for _, item := range sub.LineItems {
		if item != nil && (item.ProductId == productId || productId == "") {
			if price := playSalesRecurringPrice(item); price != "" {
				return price + " (Google Play list price, before tax and discounts)"
			}
		}
	}
	return salesConfiguredUsdAmount(sku.PriceAmountUsd, "Google Play")
}

// playSalesNotice is the notice of a credited Play purchase window.
func playSalesNotice(
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	sub *PlaySubscription,
	sku *Sku,
	maxExpiryTime time.Time,
) *model.SalesPaymentNotice {
	notice := &model.SalesPaymentNotice{
		RecordKey: fmt.Sprintf("play_purchase/%s/%d", playSubscriptionRenewal.PurchaseToken, maxExpiryTime.Unix()),
		NetworkId: &playSubscriptionRenewal.NetworkId,
		Kind:      playSalesKind(sub),
		Amount:    playSalesAmount(sub, sku, playSubscriptionRenewal.SubscriptionId),
		Method:    "Google Play",
	}
	if sku.Supporter {
		notice.Item = model.SalesProItem(salesPlanForProductId(playSubscriptionRenewal.SubscriptionId))
	} else {
		notice.Item = model.SalesDataItem(sku.BalanceByteCount())
	}
	return notice
}

// ----- x402 -----

// x402SalesNetworkName is the chain an x402 payment settled on.
func x402SalesNetworkName(network string) string {
	switch network {
	case X402NetworkBase:
		return "Base"
	case X402NetworkSolana:
		return "Solana"
	}
	return ""
}

// x402SalesNotice is the notice of a granted x402 purchase. The agent paid
// exactly the quoted price, in USDC (usdToAtomic of the sku's price).
func x402SalesNotice(networkId server.Id, sku *X402Sku, settleResponse *X402SettleResponse) *model.SalesPaymentNotice {
	notice := &model.SalesPaymentNotice{
		RecordKey: "x402/" + settleResponse.Transaction,
		NetworkId: &networkId,
		Kind:      model.SalesPaymentKindOneTime,
		Amount:    "not reported",
		Method:    "x402 USDC",
	}
	if sku.Pro {
		notice.Item = model.SalesProItem(model.PlanMonthly)
	} else {
		notice.Item = model.SalesDataItem(sku.ByteCount)
	}
	if atomic, ok := new(big.Int).SetString(usdToAtomic(sku.PriceUsd), 10); ok && 0 < sku.PriceUsd {
		notice.Amount = model.SalesAmount(atomic, UsdcDecimals, "USDC")
	}
	if name := x402SalesNetworkName(settleResponse.Network); name != "" {
		notice.Method = "x402 USDC on " + name
	}
	return notice
}

// ----- solana pay -----

// solanaSalesAmount is the USDC the transfer carried, as the chain reported it.
func solanaSalesAmount(tokenAmountUsd float64) string {
	if !(0 < tokenAmountUsd) || math.IsInf(tokenAmountUsd, 0) {
		return "not reported"
	}
	return strconv.FormatFloat(tokenAmountUsd, 'f', -1, 64) + " USDC"
}

// solanaSalesNotice is the notice of a credited Solana Pay intent: a plan or,
// from the buy-data page, a data pack.
func solanaSalesNotice(
	paymentSearchResult *model.PaymentIntentSearchResult,
	tokenAmountUsd float64,
	byteCount model.ByteCount,
) *model.SalesPaymentNotice {
	notice := &model.SalesPaymentNotice{
		RecordKey: "solana_intent/" + paymentSearchResult.PaymentReference,
		NetworkId: paymentSearchResult.NetworkId,
		Kind:      model.SalesPaymentKindOneTime,
		Amount:    solanaSalesAmount(tokenAmountUsd),
		Method:    "USDC on Solana (Solana Pay)",
	}
	switch {
	case 0 < byteCount:
		notice.Item = model.SalesDataItem(byteCount)
	case paymentSearchResult.SubscriptionPlan == model.SolanaPlanMonthly:
		notice.Item = model.SalesProItem(model.PlanMonthly)
	case paymentSearchResult.SubscriptionPlan == model.SolanaPlanYearlyOnboarding:
		notice.Item = model.SalesProItem(model.PlanYearly)
		notice.Kind = "one-time, welcome offer"
	default:
		// an intent from before the plan was recorded is a year
		notice.Item = model.SalesProItem(model.PlanYearly)
	}
	return notice
}

// ----- crypto invoices -----

func cryptoSalesSymbol(asset string) string {
	switch asset {
	case model.CryptoAssetXmr:
		return "XMR"
	case model.CryptoAssetZec:
		return "ZEC"
	case model.CryptoAssetUsdcSolana:
		return "USDC"
	}
	return ""
}

func cryptoSalesMethod(asset string) string {
	switch asset {
	case model.CryptoAssetXmr:
		return "Monero"
	case model.CryptoAssetZec:
		return "Zcash (shielded)"
	case model.CryptoAssetUsdcSolana:
		return "USDC on Solana (crypto invoice)"
	}
	return "crypto"
}

// cryptoSalesNotice is the notice of a credited crypto invoice: the amount it
// was quoted in its coin, and the USD value the credit recorded.
func cryptoSalesNotice(intent *model.CryptoPaymentIntent, credit *cryptoCredit, plan bool, byteCount model.ByteCount) *model.SalesPaymentNotice {
	notice := &model.SalesPaymentNotice{
		RecordKey: "crypto_intent/" + intent.IntentId.String(),
		NetworkId: intent.NetworkId,
		Kind:      model.SalesPaymentKindOneTime,
		Method:    cryptoSalesMethod(intent.Asset),
	}
	switch {
	case plan && intent.Item == model.SolanaPlanYearlyOnboarding:
		notice.Item = model.SalesProItem(model.PlanYearly)
		notice.Kind = "one-time, welcome offer"
	case plan:
		notice.Item = model.SalesProItem(salesPlanForProductId(intent.Item))
	default:
		notice.Item = model.SalesDataItem(byteCount)
	}
	usd := salesNanoCentsUsd(credit.usdNanoCents)
	if symbol := cryptoSalesSymbol(intent.Asset); symbol != "" && 0 < intent.AmountAtomic {
		notice.Amount = fmt.Sprintf(
			"%s %s invoice (credited as %s)",
			cryptoFormatAtomic(intent.AmountAtomic, cryptoAssetDecimals(intent.Asset)),
			symbol,
			usd,
		)
	} else {
		notice.Amount = fmt.Sprintf("credited as %s", usd)
	}
	if credit.late {
		notice.Amount += ", paid late"
	}
	return notice
}
