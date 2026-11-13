package controller

// The recognition of Shopify orders' Solana Pay payments
// (solana_shopify_order.go): the payment's total, the match against the
// store's order record, the webhook's and the receiver leg's outcomes, and
// the Admin API client against a local server. Hermetic: no database, no
// network; every account, order and credential is synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// solanaShopifyTestStore is the store's order record and the recorded order
// payments, in memory.
type solanaShopifyTestStore struct {
	orders []*solanaShopifyOrder
	// an error every lookup answers
	lookupErr error
	lookups   int
	// signature -> its recorded payment
	recorded map[string]*model.SolanaShopifyOrderPayment
}

// newSolanaShopifyTestStore replaces the order lookup and the recorded
// payments with an empty store for the rest of the test.
func newSolanaShopifyTestStore(t *testing.T) *solanaShopifyTestStore {
	store := &solanaShopifyTestStore{recorded: map[string]*model.SolanaShopifyOrderPayment{}}
	previousOrders := solanaShopifyOrdersAround
	previousRecord := solanaShopifyRecord
	previousRecorded := heliusShopifyOrderRecorded
	solanaShopifyOrdersAround = func(ctx context.Context, at time.Time, settings *SolanaShopifyOrderSettings) ([]*solanaShopifyOrder, bool, error) {
		store.lookups += 1
		if store.lookupErr != nil {
			return nil, true, store.lookupErr
		}
		orders := []*solanaShopifyOrder{}
		for _, order := range store.orders {
			for _, sale := range order.transactions {
				if gap := sale.processedAt.Sub(at); -settings.MatchWindow <= gap && gap <= settings.MatchWindow {
					orders = append(orders, order)
					break
				}
			}
		}
		return orders, true, nil
	}
	// as the table: idempotent on the signature, one payment per order
	solanaShopifyRecord = func(connOwner server.PgConn, ctx context.Context, payment *model.SolanaShopifyOrderPayment) bool {
		if existing, ok := store.recorded[payment.TxSignature]; ok {
			return existing.OrderId == payment.OrderId
		}
		for _, existing := range store.recorded {
			if existing.OrderId == payment.OrderId {
				return false
			}
		}
		copied := *payment
		store.recorded[payment.TxSignature] = &copied
		return true
	}
	heliusShopifyOrderRecorded = func(ctx context.Context, signature string) bool {
		_, ok := store.recorded[signature]
		return ok
	}
	t.Cleanup(func() {
		solanaShopifyOrdersAround = previousOrders
		solanaShopifyRecord = previousRecord
		heliusShopifyOrderRecorded = previousRecorded
	})
	return store
}

// order adds an order paid with Solana Pay: a successful sale of totalCents
// USD processed at processedAt.
func (self *solanaShopifyTestStore) order(name string, totalCents int64, processedAt time.Time) *solanaShopifyOrder {
	order := &solanaShopifyOrder{
		id:       "gid://shopify/Order/synthetic-" + strings.TrimPrefix(name, "#"),
		name:     name,
		gateways: []string{solanaShopifyGateway},
		transactions: []*solanaShopifyOrderTransaction{{
			kind:        "SALE",
			status:      "SUCCESS",
			gateway:     solanaShopifyGateway,
			processedAt: processedAt,
			paymentId:   "synthetic-payment-" + strings.TrimPrefix(name, "#"),
			amountCents: totalCents,
			currency:    "USD",
		}},
	}
	self.orders = append(self.orders, order)
	return order
}

// orderNames are the recorded payments, signature to order name.
func (self *solanaShopifyTestStore) orderNames() map[string]string {
	names := map[string]string{}
	for signature, payment := range self.recorded {
		names[signature] = payment.OrderName
	}
	return names
}

// solanaShopifyTestShop makes the store's order record one order paid with
// Solana Pay, a sale of totalCents USD processed at processedAt, for the rest
// of the test, and lists what is recorded as Shopify orders' payments,
// signature to order name.
func solanaShopifyTestShop(t *testing.T, orderName string, totalCents int64, processedAt time.Time) func() map[string]string {
	store := newSolanaShopifyTestStore(t)
	store.order(orderName, totalCents, processedAt)
	return store.orderNames
}

// The receiver and its token account of the webhook's tests.
const (
	solanaShopifyTestReceiver      = "SynthReceiverWa11et99999999999999999999999"
	solanaShopifyTestReceiverToken = "SynthReceiverToken9999999999999999999999999"
)

// solanaShopifyTestWebhook makes the test receiver ours and installs the
// webhook's unfulfilled store, for the rest of the test.
func solanaShopifyTestWebhook(t *testing.T, receiverLegRuns bool) *solanaFakeUnfulfilledStore {
	previousReceivers := solanaReceiverAddresses
	previousLegRuns := heliusReceiverLegRuns
	solanaReceiverAddresses = []string{solanaShopifyTestReceiver}
	heliusReceiverLegRuns = func() bool { return receiverLegRuns }
	t.Cleanup(func() {
		solanaReceiverAddresses = previousReceivers
		heliusReceiverLegRuns = previousLegRuns
	})
	store := &solanaFakeUnfulfilledStore{searchResults: map[string]*model.PaymentIntentSearchResult{}}
	store.install(t)
	return store
}

// The payment's total is everything the token account that paid our
// receiver sent out in the transaction, our share and the app's fees, read
// alike from Helius's enhanced transaction and from the chain's jsonParsed
// one; the transfers into our receiver, which every decision reads, are
// unchanged.
func TestSolanaShopifyPaymentTotalIsAllItsSourcePaid(t *testing.T) {
	previousReceivers := solanaReceiverAddresses
	solanaReceiverAddresses = []string{solanaShopifyTestReceiver}
	t.Cleanup(func() { solanaReceiverAddresses = previousReceivers })
	blockTime := time.Date(2026, 11, 13, 10, 15, 2, 0, time.UTC)

	helius := solanaShopifyTestHeliusPayment("total", blockTime, solanaShopifyTestReceiver, solanaShopifyTestReceiverToken, 142_530_300, 1_295_730, 143_970)
	total, ok := solanaShopifyPaymentTotalMicro(helius)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, total, int64(143_970_000))
	connect.AssertEqual(t, solanaShopifyAppPayment(helius), true)

	fake := solanaShopifyTestParsedPayment("total", 7, blockTime, solanaShopifyTestReceiver, solanaShopifyTestReceiverToken, 142_530_300, 1_295_730, 143_970)
	encoded, err := json.Marshal(fake.parsed)
	connect.AssertEqual(t, err, nil)
	var parsed *solanaParsedTransaction
	connect.AssertEqual(t, json.Unmarshal(encoded, &parsed), nil)
	transaction := solanaTransactionFromParsed("total", parsed)
	total, ok = solanaShopifyPaymentTotalMicro(transaction)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, total, int64(143_970_000))
	connect.AssertEqual(t, solanaShopifyAppPayment(transaction), true)
	received := solanaReceivedUsdcTransfers(transaction)
	connect.AssertEqual(t, len(received), 1)
	connect.AssertEqual(t, received[0].TokenAmount, 142.5303)
	connect.AssertEqual(t, received[0].FromTokenAccount, solanaShopifyTestBuyerToken)
	connect.AssertEqual(t, received[0].FromUserAccount, solanaShopifyTestBuyer)

	// a plain payment from a wallet is its own total, and runs no app
	plain := solanaReceiverFakeTransfer("plain", 8, blockTime, "SynthPayer", "SynthPayerToken", solanaShopifyTestReceiver, solanaShopifyTestReceiverToken, 40_000_000)
	encoded, err = json.Marshal(plain.parsed)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, json.Unmarshal(encoded, &parsed), nil)
	transaction = solanaTransactionFromParsed("plain", parsed)
	total, ok = solanaShopifyPaymentTotalMicro(transaction)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, total, int64(40_000_000))
	connect.AssertEqual(t, solanaShopifyAppPayment(transaction), false)

	// a transfer into our receiver that names no source has no total
	helius.TokenTransfers[0].FromTokenAccount = ""
	_, ok = solanaShopifyPaymentTotalMicro(helius)
	connect.AssertEqual(t, ok, false)
}

// An order is matched only when it is the one order paid through Solana Pay
// with a successful sale of exactly the payment's total, in USD, processed
// within the window of the block.
func TestSolanaShopifyOrderMatchNeedsOneExactSaleInTheWindow(t *testing.T) {
	settings := DefaultSolanaShopifyOrderSettings()
	blockTime := time.Date(2026, 11, 13, 10, 15, 2, 0, time.UTC)
	order := func(name string, edit func(order *solanaShopifyOrder, sale *solanaShopifyOrderTransaction)) *solanaShopifyOrder {
		sale := &solanaShopifyOrderTransaction{kind: "SALE", status: "SUCCESS", gateway: "Solana Pay", processedAt: blockTime.Add(-time.Minute), paymentId: "p-" + name, amountCents: 14397, currency: "USD"}
		made := &solanaShopifyOrder{id: "gid://shopify/Order/" + name, name: name, gateways: []string{"Solana Pay"}, transactions: []*solanaShopifyOrderTransaction{sale}}
		if edit != nil {
			edit(made, sale)
		}
		return made
	}
	for _, c := range []struct {
		name   string
		orders []*solanaShopifyOrder
		total  int64
		match  string
	}{
		{"exact", []*solanaShopifyOrder{order("#1", nil)}, 143_970_000, "#1"},
		{"a capture", []*solanaShopifyOrder{order("#1", func(o *solanaShopifyOrder, s *solanaShopifyOrderTransaction) { s.kind = "CAPTURE" })}, 143_970_000, "#1"},
		{"a cent off", []*solanaShopifyOrder{order("#1", nil)}, 143_960_000, ""},
		{"not a whole cent", []*solanaShopifyOrder{order("#1", nil)}, 143_970_001, ""},
		{"a failed sale", []*solanaShopifyOrder{order("#1", func(o *solanaShopifyOrder, s *solanaShopifyOrderTransaction) { s.status = "FAILURE" })}, 143_970_000, ""},
		{"a refund", []*solanaShopifyOrder{order("#1", func(o *solanaShopifyOrder, s *solanaShopifyOrderTransaction) { s.kind = "REFUND" })}, 143_970_000, ""},
		{"another gateway", []*solanaShopifyOrder{order("#1", func(o *solanaShopifyOrder, s *solanaShopifyOrderTransaction) {
			o.gateways = []string{"shopify_payments"}
			s.gateway = "shopify_payments"
		})}, 143_970_000, ""},
		{"another currency", []*solanaShopifyOrder{order("#1", func(o *solanaShopifyOrder, s *solanaShopifyOrderTransaction) { s.currency = "EUR" })}, 143_970_000, ""},
		{"processed long before", []*solanaShopifyOrder{order("#1", func(o *solanaShopifyOrder, s *solanaShopifyOrderTransaction) {
			s.processedAt = blockTime.Add(-settings.MatchWindow - time.Minute)
		})}, 143_970_000, ""},
		{"two orders of one total", []*solanaShopifyOrder{order("#1", nil), order("#2", nil)}, 143_970_000, ""},
		{"one of two totals", []*solanaShopifyOrder{order("#1", nil), order("#2", func(o *solanaShopifyOrder, s *solanaShopifyOrderTransaction) { s.amountCents = 10385 })}, 103_850_000, "#2"},
		{"no order", []*solanaShopifyOrder{}, 143_970_000, ""},
	} {
		matched, sale, note := solanaShopifyMatchOrder(c.orders, c.total, blockTime, settings)
		switch {
		case c.match == "" && matched != nil:
			t.Errorf("%s: matched %s", c.name, matched.name)
		case c.match != "" && (matched == nil || matched.name != c.match || sale == nil):
			t.Errorf("%s: did not match %s (%s)", c.name, c.match, note)
		case c.match == "" && note == "":
			t.Errorf("%s: no note says why nothing matched", c.name)
		}
	}
}

// Shopify's decimal amounts read as exact cents; anything else does not read.
func TestSolanaShopifyCents(t *testing.T) {
	for _, c := range []struct {
		amount string
		cents  int64
		ok     bool
	}{
		{"141.48", 14148, true},
		{"141.4", 14140, true},
		{"141", 14100, true},
		{"141.480", 14148, true},
		{"0.01", 1, true},
		{"141.485", 0, false},
		{"-141.48", 0, false},
		{"", 0, false},
		{"1e3", 0, false},
	} {
		cents, ok := solanaShopifyCents(c.amount)
		if cents != c.cents || ok != c.ok {
			t.Errorf("%q read as %d, %v; want %d, %v", c.amount, cents, ok, c.cents, c.ok)
		}
	}
}

// A Solana Pay app payment Shopify cannot verify yet (no order yet, or a
// failed lookup) is left by the webhook to the receiver leg, which reads it
// after its minimum age: neither recorded unfulfilled nor as an order. When
// the receiver leg does not run, the webhook keeps today's behaviour and
// records it unfulfilled.
func TestHeliusWebhookLeavesAnUnverifiedShopifyPaymentToTheReceiverLeg(t *testing.T) {
	blockTime := time.Date(2026, 11, 13, 10, 15, 2, 0, time.UTC)
	transaction := solanaShopifyTestHeliusPayment("not-yet", blockTime, solanaShopifyTestReceiver, solanaShopifyTestReceiverToken, 142_530_300, 1_295_730, 143_970)
	webhookSession := &session.ClientSession{Ctx: context.Background()}

	unfulfilled := solanaShopifyTestWebhook(t, true)
	shop := newSolanaShopifyTestStore(t)
	_, err := HeliusWebhook([]*SolanaTransaction{transaction}, webhookSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(unfulfilled.recorded), 0)
	connect.AssertEqual(t, len(shop.recorded), 0)
	connect.AssertEqual(t, shop.lookups, 1)

	// a failed lookup is left to the leg too, and fails nothing
	shop.lookupErr = errors.New("shopify orders: http 503")
	_, err = HeliusWebhook([]*SolanaTransaction{transaction}, webhookSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(unfulfilled.recorded), 0)

	// no receiver leg: today's behaviour
	unfulfilled = solanaShopifyTestWebhook(t, false)
	shop.lookupErr = nil
	_, err = HeliusWebhook([]*SolanaTransaction{transaction}, webhookSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(unfulfilled.recorded), 1)
	connect.AssertEqual(t, unfulfilled.recorded[0].Reason, model.SolanaUnfulfilledReasonNoIntent)
	connect.AssertEqual(t, len(shop.recorded), 0)
}

// A payment the Solana Pay app did not build is never looked up in Shopify
// and keeps today's behaviour; a redelivery of a recorded order payment is
// taken at once, with no lookup.
func TestHeliusWebhookLooksUpOnlyTheSolanaPayAppsPayments(t *testing.T) {
	blockTime := time.Date(2026, 11, 13, 10, 15, 2, 0, time.UTC)
	webhookSession := &session.ClientSession{Ctx: context.Background()}
	unfulfilled := solanaShopifyTestWebhook(t, true)
	shop := newSolanaShopifyTestStore(t)
	shop.order("#1003", 14397, blockTime.Add(-time.Minute))

	// the same amounts from a wallet, without the app's program
	plain := solanaShopifyTestHeliusPayment("plain", blockTime, solanaShopifyTestReceiver, solanaShopifyTestReceiverToken, 142_530_300, 1_295_730, 143_970)
	plain.Instructions = plain.Instructions[1:]
	_, err := HeliusWebhook([]*SolanaTransaction{plain}, webhookSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, shop.lookups, 0)
	connect.AssertEqual(t, len(unfulfilled.recorded), 1)
	connect.AssertEqual(t, len(shop.recorded), 0)

	paid := solanaShopifyTestHeliusPayment("paid", blockTime, solanaShopifyTestReceiver, solanaShopifyTestReceiverToken, 142_530_300, 1_295_730, 143_970)
	_, err = HeliusWebhook([]*SolanaTransaction{paid}, webhookSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, shop.lookups, 1)
	connect.AssertEqual(t, shop.orderNames(), map[string]string{"paid": "#1003"})
	connect.AssertEqual(t, shop.recorded["paid"].OrderTotalUsd, 143.97)
	connect.AssertEqual(t, shop.recorded["paid"].ReceivedUsd, 142.5303)
	connect.AssertEqual(t, shop.recorded["paid"].Leg, model.SolanaShopifyOrderLegWebhook)
	_, err = HeliusWebhook([]*SolanaTransaction{paid}, webhookSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, shop.lookups, 1)
	connect.AssertEqual(t, len(unfulfilled.recorded), 1)
}

// The receiver leg keeps a Solana Pay app payment no order verifies as
// no_intent (it pages), with why in its note; a second payment of an order
// another payment holds is one of those.
func TestSolanaReceiverLegKeepsAnUnverifiedShopifyPaymentUnfulfilled(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	shop := newSolanaShopifyTestStore(t)
	paidAt := solanaReceiverTestNow.Add(-3 * time.Hour)
	shop.order("#1004", 14397, paidAt.Add(-time.Minute))
	test.history(
		// the same order paid again an hour later
		solanaShopifyTestParsedPayment("paid-again", 9, paidAt.Add(time.Hour), solanaReceiverTestReceiver, solanaReceiverTestTokenAccount, 142_530_300, 1_295_730, 143_970),
		// an app payment of a total no order has
		solanaShopifyTestParsedPayment("no-order", 8, paidAt.Add(30*time.Minute), solanaReceiverTestReceiver, solanaReceiverTestTokenAccount, 99_000_000, 900_000, 100_000),
		solanaShopifyTestParsedPayment("paid", 7, paidAt, solanaReceiverTestReceiver, solanaReceiverTestTokenAccount, 142_530_300, 1_295_730, 143_970),
	)
	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, shop.orderNames(), map[string]string{"paid": "#1004"})
	connect.AssertEqual(t, shop.recorded["paid"].Leg, model.SolanaShopifyOrderLegReceiver)
	connect.AssertEqual(t, len(test.credits), 0)
	connect.AssertEqual(t, len(test.records), 2)
	notes := map[string]string{}
	for _, record := range test.records {
		connect.AssertEqual(t, record.Reason, model.SolanaUnfulfilledReasonNoIntent)
		notes[record.TxSignature] = *record.MatchNote
	}
	connect.AssertEqual(t, strings.Contains(notes["no-order"], "no Shopify order paid through Solana Pay"), true)
	connect.AssertEqual(t, strings.Contains(notes["paid-again"], "already paid by another recorded payment"), true)
	connect.AssertEqual(t, test.cursorSignature(), "paid-again")
}

// A failed order lookup is the receiver leg's error: nothing is recorded, the
// cursor stays before the payment, and the next run reads it again and
// records it as its order's.
func TestSolanaReceiverLegReadsAgainAfterAFailedShopifyLookup(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	shop := newSolanaShopifyTestStore(t)
	paidAt := solanaReceiverTestNow.Add(-2 * time.Hour)
	shop.order("#1005", 14397, paidAt.Add(-time.Minute))
	test.history(solanaShopifyTestParsedPayment("paid", 7, paidAt, solanaReceiverTestReceiver, solanaReceiverTestTokenAccount, 142_530_300, 1_295_730, 143_970))

	shop.lookupErr = errors.New("shopify orders: http 503")
	run := test.run(false)
	reconcileSolanaReceivers(run)
	connect.AssertEqual(t, len(test.records), 0)
	connect.AssertEqual(t, len(shop.recorded), 0)
	connect.AssertEqual(t, test.cursorSignature(), "")
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Errors, 1)

	shop.lookupErr = nil
	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, shop.orderNames(), map[string]string{"paid": "#1005"})
	connect.AssertEqual(t, len(test.records), 0)
	connect.AssertEqual(t, test.cursorSignature(), "paid")
}

// The Admin API client: the store app's client credentials are exchanged for
// an access token once and the token is reused while fresh; the order lookup
// asks for the orders processed within the window of the block and reads
// their sales; a failure names the step and the status, never a credential.
func TestSolanaShopifyAdminApiReadsTheOrdersAroundABlock(t *testing.T) {
	const clientSecret = "synthetic-client-secret"
	const accessToken = "synthetic-access-token"
	blockTime := time.Date(2026, 11, 13, 10, 15, 2, 0, time.UTC)
	tokenRequests := 0
	searches := []string{}
	failOrders := false
	shopifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/admin/oauth/access_token":
			tokenRequests += 1
			form, _ := url.ParseQuery(string(body))
			if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "synthetic-client" || form.Get("client_secret") != clientSecret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprintf(w, `{"access_token":%q,"scope":"read_orders","expires_in":86399}`, accessToken)
		case "/admin/api/2026-07/graphql.json":
			if r.Header.Get("X-Shopify-Access-Token") != accessToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if failOrders {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, `{"errors":"%s"}`, accessToken)
				return
			}
			var request struct {
				Variables struct {
					Q     string `json:"q"`
					First int    `json:"first"`
				} `json:"variables"`
			}
			json.Unmarshal(body, &request)
			searches = append(searches, request.Variables.Q)
			fmt.Fprint(w, `{"data":{"orders":{"edges":[{"node":{"id":"gid://shopify/Order/synthetic-1006","name":"#1006","paymentGatewayNames":["Solana Pay"],"transactions":[{"kind":"SALE","status":"SUCCESS","gateway":"Solana Pay","processedAt":"2026-11-13T10:14:12Z","paymentId":"synthetic-payment-1006","amountSet":{"shopMoney":{"amount":"143.97","currencyCode":"USD"}}}]}}]}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(shopifyServer.Close)
	previousCredentials := solanaShopifyCredentialsFunc
	previousBaseUrl := solanaShopifyBaseUrl
	solanaShopifyCredentialsFunc = func() (*solanaShopifyCredentials, bool) {
		return &solanaShopifyCredentials{shop: "synthetic-store.example", apiVersion: "2026-07", clientId: "synthetic-client", clientSecret: clientSecret}, true
	}
	solanaShopifyBaseUrl = func(shop string) string { return shopifyServer.URL }
	solanaShopifyTokensLock.Lock()
	solanaShopifyTokens = map[string]*solanaShopifyToken{}
	solanaShopifyTokensLock.Unlock()
	t.Cleanup(func() {
		solanaShopifyCredentialsFunc = previousCredentials
		solanaShopifyBaseUrl = previousBaseUrl
		solanaShopifyTokensLock.Lock()
		solanaShopifyTokens = map[string]*solanaShopifyToken{}
		solanaShopifyTokensLock.Unlock()
	})
	settings := DefaultSolanaShopifyOrderSettings()

	for range 2 {
		orders, configured, err := solanaShopifyOrdersAround(context.Background(), blockTime, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, configured, true)
		connect.AssertEqual(t, len(orders), 1)
		connect.AssertEqual(t, orders[0].name, "#1006")
		connect.AssertEqual(t, orders[0].transactions[0].amountCents, int64(14397))
		connect.AssertEqual(t, orders[0].transactions[0].processedAt.Equal(time.Date(2026, 11, 13, 10, 14, 12, 0, time.UTC)), true)
	}
	connect.AssertEqual(t, tokenRequests, 1)
	connect.AssertEqual(t, searches[0], "processed_at:>='2026-11-13T08:15:02Z' processed_at:<='2026-11-13T12:15:02Z'")

	failOrders = true
	_, _, err := solanaShopifyOrdersAround(context.Background(), blockTime, settings)
	connect.AssertEqual(t, err != nil, true)
	connect.AssertEqual(t, err.Error(), "shopify orders: http 503")
	connect.AssertEqual(t, strings.Contains(err.Error(), accessToken) || strings.Contains(err.Error(), clientSecret), false)

	// no shopify.yml: no Shopify integration, so today's behaviour
	solanaShopifyCredentialsFunc = func() (*solanaShopifyCredentials, bool) { return nil, false }
	_, configured, err := solanaShopifyOrdersAround(context.Background(), blockTime, settings)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, configured, false)
}
