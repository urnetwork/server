package controller

// Shopify's URnode orders paid with Solana Pay. Shopify's Solana Pay app pays
// an order into the same USDC receiver our apps' purchases pay
// (solanaReceiverAddresses; owner, 2026-11-13: one wallet for now), so the
// Helius webhook and the receiver leg of the Solana reconcile
// (solana_receiver_sweep.go) see each order's payment too. It names no intent
// of ours, so it was recorded as no_intent and paged
// payment-solana-unfulfilled. It is recognized here, from data that can be
// verified, and recorded as the Shopify order's payment instead
// (solana_shopify_order_payment): never credited, never paging.
//
// A transaction is a Shopify order's payment when both hold:
//
//   - Shopify's Solana Pay app built it: a top-level instruction runs the
//     app's payment program (solanaShopifyPayProgramIds, Helio's), whose
//     single payment splits one source's USDC between our receiver and the
//     app's fee accounts. The app adds no Solana Pay reference key we could
//     know, so the program only says which payments to look up;
//   - our Shopify store's order record names it, read through the existing
//     Shopify integration and keys (vault shopify.yml, the Admin API with the
//     store app's client credentials, as the support tool reads orders):
//     exactly one order paid through the Solana Pay gateway has a successful
//     sale of exactly the payment's total, processed within MatchWindow of
//     the block (one lookup reads every order processed in the window, or
//     none is verified), and no other recorded payment holds that order. The
//     total is all the USDC the paying token account sent out in the
//     transaction, our share and the app's fee together, which is the
//     order's total to the cent (16 of 16 orders paid 2025-11 to 2026-02,
//     swaps through Jupiter included, matched their order this way and no
//     other).
//
// Anything else keeps today's behaviour: a payment no order record verifies
// is recorded as no_intent and pages. Shopify makes its order a few seconds
// after the payment's block, so the webhook, which may hear of a payment
// before that, leaves a Solana Pay app payment it cannot verify yet to the
// receiver leg (its MinAge later) whenever the leg can run, and the leg
// decides. A lookup that fails is the leg's error event: it reads the
// transaction again next run.
//
// A payment is never credited on any of this: the recognition only runs for
// a payment the decision would record as no_intent, and an app purchase never
// runs the Solana Pay app's program (the memo the app adds also keeps the
// memo-less amount match off: solana_memoless_match.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The payment program of Shopify's Solana Pay app (Helio's): a fact of the
// chain, like the memo programs.
var solanaShopifyPayProgramIds = []string{
	"ENicYBBNZQ91toN7ggmTxnDGZW14uv9UkumN7XBGeYJ4",
}

// The payment gateway Shopify names for an order paid with Solana Pay.
const solanaShopifyGateway = "Solana Pay"

// The Admin API version asked when shopify.yml names none (the support
// tool's).
const solanaShopifyDefaultApiVersion = "2026-07"

// A Shopify answer is read up to this many bytes.
const solanaShopifyMaxResponseBytes = 4 * 1024 * 1024

// The Shopify order lookup's tunables.
type SolanaShopifyOrderSettings struct {
	// How far from the payment's block an order's Solana Pay sale may have
	// been processed. The orders paid 2025-11 to 2026-02 were processed one to
	// two minutes before their block; the window leaves room for a slow
	// wallet and a skewed clock, and the match still needs the exact total.
	// Non-positive matches nothing.
	MatchWindow time.Duration
	// The most orders one lookup reads: the store's orders processed in the
	// window around the block. A window with more verifies no payment in it,
	// since an order past the lookup could hold the same total.
	OrdersPerLookup int
	// One Admin API request is abandoned after this long.
	RequestTimeout time.Duration
	// How long an access token is used when Shopify answers no expiry; one
	// is never used past the expiry Shopify answers.
	TokenTtl time.Duration
}

// A new instance on every call.
func DefaultSolanaShopifyOrderSettings() *SolanaShopifyOrderSettings {
	return &SolanaShopifyOrderSettings{
		MatchWindow:     2 * time.Hour,
		OrdersPerLookup: 50,
		RequestTimeout:  30 * time.Second,
		TokenTtl:        time.Hour,
	}
}

// Shopify order payments the webhook and the receiver leg looked at, by leg
// and outcome: recorded (the order's payment), unverified (no order record
// verifies it: recorded as no_intent), deferred (left by the webhook to the
// receiver leg), error (the lookup failed).
var solanaShopifyOrderPayments = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "solana",
	Name:      "shopify_order_payments_total",
	Help:      "Solana Pay payments of Shopify orders into our USDC receivers, by leg (webhook, receiver) and outcome (recorded, unverified, deferred, error)",
}, []string{"leg", "outcome"})

func init() {
	prometheus.MustRegister(solanaShopifyOrderPayments)
}

// An order of the store as the lookup reads it.
type solanaShopifyOrder struct {
	id           string
	name         string
	gateways     []string
	transactions []*solanaShopifyOrderTransaction
}

// One of an order's payment transactions.
type solanaShopifyOrderTransaction struct {
	kind        string
	status      string
	gateway     string
	processedAt time.Time
	paymentId   string
	// the shop's money: cents and its currency
	amountCents int64
	currency    string
}

// The store's orders processed within the match window of at. configured =
// false when no Shopify integration is configured (vault shopify.yml).
// errSolanaShopifyOrdersPastLookup: the window holds more orders than one
// lookup reads. Replaceable only by tests.
var solanaShopifyOrdersAround = func(ctx context.Context, at time.Time, settings *SolanaShopifyOrderSettings) (orders []*solanaShopifyOrder, configured bool, returnErr error) {
	credentials, ok := solanaShopifyCredentialsFunc()
	if !ok {
		return nil, false, nil
	}
	orders, err := solanaShopifyReadOrders(ctx, credentials, at, settings)
	return orders, true, err
}

// The window of an order lookup holds more orders than one lookup reads, so
// no order it read is known to be the only one of its total.
var errSolanaShopifyOrdersPastLookup = errors.New("shopify orders: more orders in the window than one lookup reads")

// The write of a recognized payment. Replaceable only by tests.
var solanaShopifyRecord = model.RecordSolanaShopifyOrderPaymentInConn

// Whether Shopify's Solana Pay app built a transaction: one of its top-level
// instructions runs the app's payment program.
func solanaShopifyAppPayment(transaction *SolanaTransaction) bool {
	for _, instruction := range transaction.Instructions {
		if slices.Contains(solanaShopifyPayProgramIds, instruction.ProgramId) {
			return true
		}
	}
	return false
}

// The USDC the payment's source sent out in the transaction, in micro-USDC:
// every USDC transfer from the token account that paid our receiver, our share
// and the app's fee together. ok = false when no transfer to our receiver
// names its source, or two sources paid it.
func solanaShopifyPaymentTotalMicro(transaction *SolanaTransaction) (totalMicro int64, ok bool) {
	source := ""
	for _, transfer := range solanaReceivedUsdcTransfers(transaction) {
		switch {
		case transfer.FromTokenAccount == "":
			return 0, false
		case source == "":
			source = transfer.FromTokenAccount
		case source != transfer.FromTokenAccount:
			return 0, false
		}
	}
	if source == "" {
		return 0, false
	}
	for _, transfer := range transaction.TokenTransfers {
		if transfer.Mint == solanaUsdcMint && transfer.FromTokenAccount == source && 0 < transfer.TokenAmount {
			totalMicro += model.SolanaUsdToMicro(transfer.TokenAmount)
		}
	}
	return totalMicro, 0 < totalMicro
}

// The one order whose Solana Pay sale paid totalMicro within the match window
// of the block, with that sale; nil and why not otherwise.
func solanaShopifyMatchOrder(
	orders []*solanaShopifyOrder,
	totalMicro int64,
	blockTime time.Time,
	settings *SolanaShopifyOrderSettings,
) (*solanaShopifyOrder, *solanaShopifyOrderTransaction, string) {
	if totalMicro%10_000 != 0 {
		return nil, nil, fmt.Sprintf("the payment's total %s USDC is not a whole cent", solanaFormatMicro(totalMicro))
	}
	totalCents := totalMicro / 10_000
	var matchedOrder *solanaShopifyOrder
	var matchedSale *solanaShopifyOrderTransaction
	matches := 0
	for _, order := range orders {
		if !slices.ContainsFunc(order.gateways, func(gateway string) bool { return strings.EqualFold(gateway, solanaShopifyGateway) }) {
			continue
		}
		for _, sale := range order.transactions {
			if (sale.kind != "SALE" && sale.kind != "CAPTURE") || sale.status != "SUCCESS" ||
				!strings.EqualFold(sale.gateway, solanaShopifyGateway) || sale.currency != "USD" || sale.amountCents != totalCents {
				continue
			}
			gap := sale.processedAt.Sub(blockTime)
			if gap < -settings.MatchWindow || settings.MatchWindow < gap || settings.MatchWindow <= 0 {
				continue
			}
			if matchedOrder != order {
				matches += 1
			}
			matchedOrder = order
			matchedSale = sale
		}
	}
	switch matches {
	case 0:
		return nil, nil, fmt.Sprintf("no Shopify order paid through %s was a sale of %s USD within %s of the block", solanaShopifyGateway, solanaFormatMicro(totalMicro), settings.MatchWindow)
	case 1:
		return matchedOrder, matchedSale, ""
	}
	return nil, nil, fmt.Sprintf("%d Shopify orders paid through %s were sales of %s USD within %s of the block", matches, solanaShopifyGateway, solanaFormatMicro(totalMicro), settings.MatchWindow)
}

// What recognizing one payment came to.
type solanaShopifyOutcome int

const (
	// not a Solana Pay app payment, or no Shopify integration configured:
	// today's behaviour
	solanaShopifyNotApplicable solanaShopifyOutcome = iota
	// recorded as its Shopify order's payment
	solanaShopifyRecorded
	// a Solana Pay app payment no order record verifies (none, two, or one
	// another payment holds): today's behaviour
	solanaShopifyUnverified
)

// Recognizes a payment the decision would record as no_intent (see the file
// header) and records it as its Shopify order's payment. A dry run writes
// nothing. note says why a Solana Pay app payment was not verified. err: the
// order lookup failed, nothing was written.
func solanaRecognizeShopifyOrder(
	connOwner server.PgConn,
	ctx context.Context,
	transaction *SolanaTransaction,
	receivedUsd float64,
	leg string,
	settings *SolanaShopifyOrderSettings,
	dryRun bool,
) (outcome solanaShopifyOutcome, note string, returnErr error) {
	if !solanaShopifyAppPayment(transaction) {
		return solanaShopifyNotApplicable, "", nil
	}
	if transaction.Timestamp == 0 {
		return solanaShopifyUnverified, "the Solana Pay app payment has no on-chain time", nil
	}
	blockTime := time.Unix(transaction.Timestamp, 0).UTC()
	totalMicro, ok := solanaShopifyPaymentTotalMicro(transaction)
	if !ok {
		return solanaShopifyUnverified, "the Solana Pay app payment's source is not named", nil
	}
	orders, configured, err := solanaShopifyOrdersAround(ctx, blockTime, settings)
	if !configured {
		return solanaShopifyNotApplicable, "", nil
	}
	if errors.Is(err, errSolanaShopifyOrdersPastLookup) {
		// final for this payment: the orders of a window it read do not go away
		return solanaShopifyUnverified, fmt.Sprintf("more than %d Shopify orders were processed within %s of the block, so no one order is verified as the payment's", settings.OrdersPerLookup, settings.MatchWindow), nil
	}
	if err != nil {
		return solanaShopifyUnverified, "", err
	}
	order, sale, note := solanaShopifyMatchOrder(orders, totalMicro, blockTime, settings)
	if order == nil {
		return solanaShopifyUnverified, note, nil
	}
	if dryRun {
		return solanaShopifyRecorded, "", nil
	}
	recorded := solanaShopifyRecord(connOwner, ctx, &model.SolanaShopifyOrderPayment{
		TxSignature:     transaction.Signature,
		OrderId:         order.id,
		OrderName:       order.name,
		PaymentId:       sale.paymentId,
		ReceivedUsd:     receivedUsd,
		OrderTotalUsd:   float64(sale.amountCents) / 100,
		TransactionTime: &blockTime,
		Leg:             leg,
	})
	if !recorded {
		return solanaShopifyUnverified, fmt.Sprintf("Shopify order %s is already paid by another recorded payment", order.name), nil
	}
	glog.Infof("[solana]a payment into our receiver is Shopify order %s, paid with Solana Pay (%s leg); recorded as the order's payment\n", order.name, leg)
	return solanaShopifyRecorded, "", nil
}

// ----- the Shopify Admin API -----

// The Admin API's credentials: vault shopify.yml, the store app's client
// credentials (the support tool's), exchanged for a short-lived access token
// that is never stored, logged or written into an error.
type solanaShopifyCredentials struct {
	shop         string
	apiVersion   string
	clientId     string
	clientSecret string
}

// The credentials; ok = false when the vault names none. Replaceable only
// by tests.
var solanaShopifyCredentialsFunc = func() (*solanaShopifyCredentials, bool) {
	resource, err := server.Vault.SimpleResource("shopify.yml")
	if err != nil {
		return nil, false
	}
	credentials := &solanaShopifyCredentials{}
	for _, field := range []struct {
		key   string
		value *string
	}{
		{"shop", &credentials.shop},
		{"client_id", &credentials.clientId},
		{"client_secret", &credentials.clientSecret},
	} {
		value, err := simpleResourceNonblankStringValue(resource, field.key)
		if err != nil {
			glog.Warningf("[solana]shopify.yml is incomplete (%s), so no Solana Pay payment is matched to its Shopify order\n", field.key)
			return nil, false
		}
		*field.value = strings.TrimSpace(value)
	}
	credentials.apiVersion = solanaShopifyDefaultApiVersion
	if apiVersion, err := simpleResourceNonblankStringValue(resource, "api_version"); err == nil {
		credentials.apiVersion = strings.TrimSpace(apiVersion)
	}
	// a bare host name, never a path or a scheme
	if strings.ContainsAny(credentials.shop, "/:?#@ ") {
		glog.Warningf("[solana]shopify.yml names a shop that is not a host name, so no Solana Pay payment is matched to its Shopify order\n")
		return nil, false
	}
	return credentials, true
}

// The Admin API's base url of a shop. Replaceable only by tests, which serve
// it locally.
var solanaShopifyBaseUrl = func(shop string) string {
	return "https://" + shop
}

// An access token and when it is no longer used.
type solanaShopifyToken struct {
	value     string
	expiresAt time.Time
}

// The access tokens of this process, by shop and client.
var solanaShopifyTokensLock sync.Mutex
var solanaShopifyTokens = map[string]*solanaShopifyToken{}

// Posts a request and reads its answer into out. Errors name the step and the
// status only.
func solanaShopifyPost(ctx context.Context, settings *SolanaShopifyOrderSettings, step string, requestUrl string, contentType string, body string, headers map[string]string, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, settings.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, requestUrl, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("shopify %s: invalid request", step)
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := server.NewHttpClient(settings.RequestTimeout).Do(request)
	if err != nil {
		return fmt.Errorf("shopify %s: transport failed", step)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, solanaShopifyMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("shopify %s: read failed", step)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("shopify %s: http %d", step, response.StatusCode)
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("shopify %s: undecodable answer", step)
	}
	return nil
}

// An access token for the credentials: the cached one while it is fresh, else
// a new one from the client credentials grant.
func solanaShopifyAccessToken(ctx context.Context, credentials *solanaShopifyCredentials, settings *SolanaShopifyOrderSettings) (string, error) {
	key := credentials.shop + "/" + credentials.clientId
	now := time.Now()
	var cached *solanaShopifyToken
	func() {
		solanaShopifyTokensLock.Lock()
		defer solanaShopifyTokensLock.Unlock()
		cached = solanaShopifyTokens[key]
	}()
	if cached != nil && now.Before(cached.expiresAt) {
		return cached.value, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {credentials.clientId},
		"client_secret": {credentials.clientSecret},
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := solanaShopifyPost(ctx, settings, "token", solanaShopifyBaseUrl(credentials.shop)+"/admin/oauth/access_token", "application/x-www-form-urlencoded", form.Encode(), nil, &answer); err != nil {
		return "", err
	}
	if answer.AccessToken == "" {
		return "", errors.New("shopify token: no access token in the answer")
	}
	lifetime := settings.TokenTtl
	if 0 < answer.ExpiresIn {
		// a minute short of what Shopify answered
		lifetime = min(lifetime, time.Duration(answer.ExpiresIn)*time.Second-time.Minute)
	}
	token := &solanaShopifyToken{value: answer.AccessToken, expiresAt: now.Add(lifetime)}
	func() {
		solanaShopifyTokensLock.Lock()
		defer solanaShopifyTokensLock.Unlock()
		solanaShopifyTokens[key] = token
	}()
	return token.value, nil
}

// The order lookup: the store's orders processed in a window, with their
// payment transactions, and whether the window holds more.
const solanaShopifyOrdersQuery = `query($q: String!, $first: Int!) { orders(first: $first, query: $q, sortKey: PROCESSED_AT) { edges { node {
  id name paymentGatewayNames
  transactions(first: 20) { kind status gateway processedAt paymentId amountSet { shopMoney { amount currencyCode } } }
} } pageInfo { hasNextPage } } }`

// The store's orders processed within the match window of at;
// errSolanaShopifyOrdersPastLookup when the window holds more than
// OrdersPerLookup.
func solanaShopifyReadOrders(ctx context.Context, credentials *solanaShopifyCredentials, at time.Time, settings *SolanaShopifyOrderSettings) ([]*solanaShopifyOrder, error) {
	token, err := solanaShopifyAccessToken(ctx, credentials, settings)
	if err != nil {
		return nil, err
	}
	search := fmt.Sprintf(
		"processed_at:>='%s' processed_at:<='%s'",
		at.Add(-settings.MatchWindow).UTC().Format(time.RFC3339),
		at.Add(settings.MatchWindow).UTC().Format(time.RFC3339),
	)
	request, err := json.Marshal(map[string]any{
		"query":     solanaShopifyOrdersQuery,
		"variables": map[string]any{"q": search, "first": settings.OrdersPerLookup},
	})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Data struct {
			Orders struct {
				Edges []struct {
					Node struct {
						Id                  string   `json:"id"`
						Name                string   `json:"name"`
						PaymentGatewayNames []string `json:"paymentGatewayNames"`
						Transactions        []struct {
							Kind        string `json:"kind"`
							Status      string `json:"status"`
							Gateway     string `json:"gateway"`
							ProcessedAt string `json:"processedAt"`
							PaymentId   string `json:"paymentId"`
							AmountSet   struct {
								ShopMoney struct {
									Amount       string `json:"amount"`
									CurrencyCode string `json:"currencyCode"`
								} `json:"shopMoney"`
							} `json:"amountSet"`
						} `json:"transactions"`
					} `json:"node"`
				} `json:"edges"`
				PageInfo struct {
					HasNextPage bool `json:"hasNextPage"`
				} `json:"pageInfo"`
			} `json:"orders"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	graphqlUrl := solanaShopifyBaseUrl(credentials.shop) + "/admin/api/" + url.PathEscape(credentials.apiVersion) + "/graphql.json"
	if err := solanaShopifyPost(ctx, settings, "orders", graphqlUrl, "application/json", string(request), map[string]string{"X-Shopify-Access-Token": token}, &answer); err != nil {
		return nil, err
	}
	if 0 < len(answer.Errors) {
		return nil, fmt.Errorf("shopify orders: %d graphql errors", len(answer.Errors))
	}
	if answer.Data.Orders.PageInfo.HasNextPage {
		return nil, errSolanaShopifyOrdersPastLookup
	}
	orders := []*solanaShopifyOrder{}
	for _, edge := range answer.Data.Orders.Edges {
		node := edge.Node
		order := &solanaShopifyOrder{id: node.Id, name: node.Name, gateways: node.PaymentGatewayNames}
		for _, transaction := range node.Transactions {
			cents, ok := solanaShopifyCents(transaction.AmountSet.ShopMoney.Amount)
			if !ok {
				continue
			}
			processedAt, err := time.Parse(time.RFC3339, transaction.ProcessedAt)
			if err != nil {
				continue
			}
			order.transactions = append(order.transactions, &solanaShopifyOrderTransaction{
				kind:        transaction.Kind,
				status:      transaction.Status,
				gateway:     transaction.Gateway,
				processedAt: processedAt.UTC(),
				paymentId:   transaction.PaymentId,
				amountCents: cents,
				currency:    transaction.AmountSet.ShopMoney.CurrencyCode,
			})
		}
		orders = append(orders, order)
	}
	return orders, nil
}

// A Shopify decimal amount ("141.48") as cents, exactly; ok = false for
// anything else (a sign, more than two decimals that are not zeros, not a
// number).
func solanaShopifyCents(amount string) (int64, bool) {
	whole, fraction, _ := strings.Cut(strings.TrimSpace(amount), ".")
	if whole == "" || 15 < len(whole) {
		return 0, false
	}
	if 2 < len(fraction) {
		if strings.Trim(fraction[2:], "0") != "" {
			return 0, false
		}
		fraction = fraction[:2]
	}
	fraction = (fraction + "00")[:2]
	var cents int64
	for _, r := range whole + fraction {
		if r < '0' || '9' < r {
			return 0, false
		}
		cents = cents*10 + int64(r-'0')
	}
	return cents, true
}
