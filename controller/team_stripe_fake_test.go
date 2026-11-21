//go:build sidecar_pending

package controller

// A fake Stripe for Team's checkout and events (QUOTA-M6-02): prices, Checkout sessions, whose
// forms it records, invoices and subscriptions, read through the raw-HTTP seam and the stripe-go
// backend alike. Under -tags sidecar_pending, since a Team's tables are MG6's.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const (
	testTeamPriceBase     = "price_synthetic_team_base"
	testTeamPriceSeats    = "price_synthetic_team_seats"
	testTeamPriceTransfer = "price_synthetic_team_transfer"
	testTeamPriceStorage  = "price_synthetic_team_storage"
)

type teamStripeTestEnv struct {
	stateLock                sync.Mutex
	prices                   map[string]map[string]any
	invoices                 map[string]map[string]any
	listedPaidInvoiceIds     []string
	subscriptions            map[string]map[string]any
	cancellations            map[string]int
	rejectNextCancellation   bool
	checkoutForms            []url.Values
	checkouts                []map[string]any
	checkoutKeys             map[string]int
	afterCheckoutList        func()
	afterSubscriptionRead    func(string)
	loseNextCheckoutResponse bool

	testServer *httptest.Server
}

// Starts the fake with pro.yml's Team prices configured on both sides: the process's config and
// matching Stripe prices under team_prices' ids, with storage overage unpriced as it ships.
func newTeamStripeTestEnv(t testing.TB) *teamStripeTestEnv {
	env := &teamStripeTestEnv{
		prices:        map[string]map[string]any{},
		invoices:      map[string]map[string]any{},
		subscriptions: map[string]map[string]any{},
		cancellations: map[string]int{},
		checkoutKeys:  map[string]int{},
	}
	writeJson := func(w http.ResponseWriter, status int, object any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(object)
	}
	notFound := func(w http.ResponseWriter) {
		writeJson(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "not found"}})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/customers", func(w http.ResponseWriter, r *http.Request) {
		writeJson(w, http.StatusOK, map[string]any{"id": "cus_synthetic_" + r.FormValue("metadata[network_id]"), "object": "customer"})
	})
	mux.HandleFunc("GET /v1/prices/{priceId}", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		price, ok := env.prices[r.PathValue("priceId")]
		env.stateLock.Unlock()
		if !ok {
			notFound(w)
			return
		}
		writeJson(w, http.StatusOK, price)
	})
	mux.HandleFunc("POST /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			writeJson(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "bad form"}})
			return
		}
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		key := r.Header.Get("Idempotency-Key")
		if index, ok := env.checkoutKeys[key]; ok {
			if form.Encode() != env.checkoutForms[index].Encode() {
				writeJson(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "idempotency_error"}})
				return
			}
			writeJson(w, http.StatusOK, env.checkouts[index])
			return
		}
		env.checkoutKeys[key] = len(env.checkouts)
		env.checkoutForms = append(env.checkoutForms, form)
		sessionId := fmt.Sprintf("cs_synthetic_%d", len(env.checkoutForms))
		uiMode := form.Get("ui_mode")
		if uiMode == "" {
			uiMode = StripeUiModeHosted
		}
		expiresAt, _ := strconv.ParseInt(form.Get("expires_at"), 10, 64)
		s := map[string]any{
			"id":                     sessionId,
			"object":                 "checkout.session",
			"customer":               form.Get("customer"),
			"mode":                   form.Get("mode"),
			"ui_mode":                uiMode,
			"status":                 "open",
			"client_reference_id":    form.Get("client_reference_id"),
			"redirect_on_completion": form.Get("redirect_on_completion"),
			"metadata":               map[string]string{stripeMetadataTeam: form.Get("metadata[team]")},
			"expires_at":             expiresAt,
			"created":                teamCheckoutNow().Unix(),
			"url":                    "https://checkout.stripe.example/c/" + sessionId,
			"client_secret":          sessionId + "_secret",
		}
		env.checkouts = append(env.checkouts, s)
		if env.loseNextCheckoutResponse {
			env.loseNextCheckoutResponse = false
			writeJson(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"type": "api_error", "message": "synthetic lost response"}})
			return
		}
		writeJson(w, http.StatusOK, s)
	})
	mux.HandleFunc("GET /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		data := []any{}
		minCreated, _ := strconv.ParseInt(r.URL.Query().Get("created[gte]"), 10, 64)
		for i := len(env.checkouts) - 1; 0 <= i; i-- {
			s := env.checkouts[i]
			if !r.URL.Query().Has("customer") || s["customer"] == r.URL.Query().Get("customer") {
				if minCreated > 0 && s["created"].(int64) < minCreated {
					continue
				}
				if s["status"] == "open" && s["expires_at"].(int64) <= teamCheckoutNow().Unix() {
					s["status"] = "expired"
				}
				listed := maps.Clone(s)
				delete(listed, "client_secret")
				data = append(data, listed)
			}
		}
		after := env.afterCheckoutList
		env.stateLock.Unlock()
		if after != nil && r.URL.Query().Has("created[gte]") {
			after()
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
	})
	mux.HandleFunc("GET /v1/checkout/sessions/{sessionId}", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		for _, s := range env.checkouts {
			if s["id"] == r.PathValue("sessionId") {
				writeJson(w, http.StatusOK, s)
				return
			}
		}
		notFound(w)
	})
	// Only the paid invoices selected for the reconciler appear in its store-side listing.
	mux.HandleFunc("GET /v1/invoices", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		data := []any{}
		for _, id := range env.listedPaidInvoiceIds {
			invoice := env.invoices[id]
			data = append(data, map[string]any{"id": id, "total": invoice["total"], "currency": invoice["currency"], "customer": "cus_synthetic_" + invoice["subscription"].(string)})
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
	})
	mux.HandleFunc("GET /v1/invoices/{invoiceId}", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		invoice, ok := env.invoices[r.PathValue("invoiceId")]
		served := map[string]any{}
		for key, value := range invoice {
			served[key] = value
		}
		// expanded, with the subscription as it is now
		if subscriptionId, _ := invoice["subscription"].(string); subscriptionId != "" {
			served["subscription"] = maps.Clone(env.subscriptions[subscriptionId])
		}
		env.stateLock.Unlock()
		if !ok {
			notFound(w)
			return
		}
		writeJson(w, http.StatusOK, served)
	})
	mux.HandleFunc("GET /v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		sub, ok := env.subscriptions[r.PathValue("subscriptionId")]
		sub = maps.Clone(sub)
		after := env.afterSubscriptionRead
		env.stateLock.Unlock()
		if !ok {
			notFound(w)
			return
		}
		if after != nil {
			after(r.PathValue("subscriptionId"))
		}
		writeJson(w, http.StatusOK, sub)
	})
	mux.HandleFunc("DELETE /v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		id := r.PathValue("subscriptionId")
		sub := env.subscriptions[id]
		if sub == nil {
			notFound(w)
			return
		}
		if env.rejectNextCancellation {
			env.rejectNextCancellation = false
			writeJson(w, http.StatusServiceUnavailable, map[string]any{"error": "synthetic unavailable"})
			return
		}
		env.cancellations[id]++
		sub["status"] = "canceled"
		writeJson(w, http.StatusOK, sub)
	})
	mux.HandleFunc("GET /v1/subscriptions/search", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		data := []any{}
		for _, sub := range env.subscriptions {
			metadata := sub["metadata"].(map[string]any)
			q := r.URL.Query().Get("query")
			if q == fmt.Sprintf("metadata['network_id']:'%s'", metadata["network_id"]) || q == "metadata['team']:'1'" && metadata["team"] == stripeTeamMark {
				data = append(data, sub)
			}
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "search_result", "data": data, "has_more": false})
	})
	mux.HandleFunc("GET /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		defer env.stateLock.Unlock()
		data := []any{}
		for _, sub := range env.subscriptions {
			if sub["customer"] == r.URL.Query().Get("customer") {
				data = append(data, sub)
			}
		}
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
	})
	env.testServer = httptest.NewServer(mux)

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousToken := stripeApiToken
	previousPublishableKey := stripePublishableKey
	previousBackend := stripe.GetBackend(stripe.APIBackend)
	previousTeamPrices := stripeTeamPrices
	previousCheckoutUrls := stripeCheckoutUrls
	stripeApiBaseUrl = env.testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_team" }
	stripeApiToken = func() string { return "sk_test_team" }
	stripePublishableKey = func() string { return "pk_test_team" }
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(env.testServer.URL),
		MaxNetworkRetries: stripe.Int64(0),
	}))
	stripeTeamPrices = func() StripeTeamPrices {
		return StripeTeamPrices{
			Base:            testTeamPriceBase,
			Seats:           testTeamPriceSeats,
			TransferOverage: testTeamPriceTransfer,
			StorageOverage:  testTeamPriceStorage,
		}
	}
	stripeCheckoutUrls = func() StripeCheckoutUrls {
		return StripeCheckoutUrls{
			SuccessUrl: "https://ur.example/checkout/success",
			CancelUrl:  "https://ur.example/checkout/cancel",
			ReturnUrl:  "https://ur.example/checkout/return",
		}
	}
	restoreTeamPrices := model.Testing_SetTeamPrices(testTeamPrices())
	t.Cleanup(func() {
		restoreTeamPrices()
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		stripeApiToken = previousToken
		stripePublishableKey = previousPublishableKey
		stripe.SetBackend(stripe.APIBackend, previousBackend)
		stripeTeamPrices = previousTeamPrices
		stripeCheckoutUrls = previousCheckoutUrls
		env.testServer.Close()
	})

	env.setPrice(testTeamPriceBase, map[string]any{
		"billing_scheme":      "per_unit",
		"unit_amount":         10000,
		"unit_amount_decimal": "10000",
		"recurring":           map[string]any{"interval": "month", "interval_count": 1, "usage_type": "licensed"},
	})
	env.setPrice(testTeamPriceSeats, map[string]any{
		"billing_scheme": "tiered",
		"tiers_mode":     "graduated",
		"tiers": []any{
			map[string]any{"up_to": 5, "unit_amount": 0, "unit_amount_decimal": "0"},
			map[string]any{"up_to": nil, "unit_amount": 2000, "unit_amount_decimal": "2000"},
		},
		"recurring": map[string]any{"interval": "month", "interval_count": 1, "usage_type": "metered", "meter": "mtr_synthetic_seats"},
	})
	env.setPrice(testTeamPriceTransfer, map[string]any{
		"billing_scheme":      "per_unit",
		"unit_amount_decimal": "0.29296875",
		"recurring":           map[string]any{"interval": "month", "interval_count": 1, "usage_type": "metered", "meter": "mtr_synthetic_transfer"},
	})
	env.setPrice(testTeamPriceStorage, map[string]any{
		"billing_scheme":      "per_unit",
		"unit_amount_decimal": "0.065753424658",
		"recurring":           map[string]any{"interval": "month", "interval_count": 1, "usage_type": "metered", "meter": "mtr_synthetic_storage"},
	})
	return env
}

// Sets a live USD price with the given fields.
func (self *teamStripeTestEnv) setPrice(priceId string, fields map[string]any) {
	price := map[string]any{
		"id":       priceId,
		"object":   "price",
		"active":   true,
		"currency": "usd",
	}
	for key, value := range fields {
		price[key] = value
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.prices[priceId] = price
}

// The Checkout session forms posted so far.
func (self *teamStripeTestEnv) postedCheckoutForms() []url.Values {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]url.Values{}, self.checkoutForms...)
}

func (self *teamStripeTestEnv) confirmedCancellations() map[string]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Clone(self.cancellations)
}

// Sets a subscription in `status`, with the network and, for a Team's, the team mark in its
// metadata.
func (self *teamStripeTestEnv) setSubscription(subscriptionId string, networkId server.Id, team bool, status string) {
	metadata := map[string]any{"network_id": networkId.String()}
	if team {
		metadata[stripeMetadataTeam] = stripeTeamMark
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.subscriptions[subscriptionId] = map[string]any{
		"id":       subscriptionId,
		"object":   "subscription",
		"status":   status,
		"customer": "cus_synthetic_" + subscriptionId,
		"metadata": metadata,
		"items":    map[string]any{"object": "list", "data": []any{}},
	}
}

func (self *teamStripeTestEnv) setSubscriptionStatus(subscriptionId string, status string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.subscriptions[subscriptionId]["status"] = status
}

// An invoice line of a subscription item.
func testTeamInvoiceLine(subscriptionId string, priceId string, amount int64, start time.Time, end time.Time) map[string]any {
	return map[string]any{
		"id":           "il_" + server.NewId().String(),
		"object":       "line_item",
		"type":         "subscription",
		"subscription": subscriptionId,
		"amount":       amount,
		"currency":     "usd",
		"price":        map[string]any{"id": priceId},
		"period":       map[string]any{"start": start.Unix(), "end": end.Unix()},
	}
}

// Sets the subscription's invoice as Stripe reports it now. An open invoice has one failed
// attempt and another to come.
func (self *teamStripeTestEnv) setInvoice(invoiceId string, subscriptionId string, status string, billingReason string, total int64, lines ...map[string]any) {
	created := server.NowUtc().Add(-time.Hour)
	invoice := map[string]any{
		"id":                 invoiceId,
		"object":             "invoice",
		"status":             status,
		"paid":               status == "paid",
		"billing_reason":     billingReason,
		"attempt_count":      0,
		"hosted_invoice_url": "https://invoice.stripe.com/i/acct_synthetic/" + invoiceId,
		"amount_due":         total,
		"total":              total,
		"currency":           "usd",
		"created":            created.Unix(),
		"customer":           map[string]any{"id": "cus_synthetic_" + subscriptionId, "object": "customer", "email": ""},
		"subscription":       subscriptionId,
		"lines":              map[string]any{"object": "list", "data": lines, "has_more": false},
	}
	if status == "open" {
		invoice["attempt_count"] = 1
		invoice["next_payment_attempt"] = created.Add(3 * 24 * time.Hour).Unix()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.invoices[invoiceId] = invoice
}

// A team network, as team-create leaves it, and a session of its owner of record's network
// credential.
func newTeamStripeTestNetwork(t testing.TB, ctx context.Context) (server.Id, *session.ClientSession) {
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "team-"+networkId.String(), userId)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id) VALUES ($1)`, networkId))
	})
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
	return networkId, clientSession
}

// Delivers an invoice event of the invoice as the fake holds it.
func (self *teamStripeTestEnv) deliverInvoiceEvent(t testing.TB, clientSession *session.ClientSession, eventType string, invoiceId string) {
	t.Helper()
	self.stateLock.Lock()
	invoice := self.invoices[invoiceId]
	object := map[string]any{
		"id":             invoiceId,
		"object":         "invoice",
		"total":          invoice["total"],
		"currency":       invoice["currency"],
		"billing_reason": invoice["billing_reason"],
		"status":         invoice["status"],
		"customer":       "cus_synthetic_" + invoice["subscription"].(string),
	}
	self.stateLock.Unlock()
	result, err := StripeWebhook(stripeWebhookEvent(t, eventType, object), clientSession)
	if err != nil || result == nil {
		t.Fatalf("%s for %s: %+v %v", eventType, invoiceId, result, err)
	}
}

// Delivers customer.subscription.deleted of the subscription as the fake holds it.
func (self *teamStripeTestEnv) deliverSubscriptionDeleted(t testing.TB, clientSession *session.ClientSession, subscriptionId string) {
	t.Helper()
	self.stateLock.Lock()
	object := map[string]any{}
	for key, value := range self.subscriptions[subscriptionId] {
		object[key] = value
	}
	self.stateLock.Unlock()
	result, err := StripeWebhook(stripeWebhookEvent(t, "customer.subscription.deleted", object), clientSession)
	if err != nil || result == nil {
		t.Fatalf("customer.subscription.deleted for %s: %+v %v", subscriptionId, result, err)
	}
}

// A transfer balance of the network, as the tests read it.
type testTeamBalance struct {
	grantKind string
	tier      string
	pro       bool
	startTime time.Time
	endTime   time.Time
}

func testTeamBalances(t testing.TB, ctx context.Context, networkId server.Id) (balances []*testTeamBalance) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT COALESCE(grant_kind, ''), COALESCE(tier, ''), pro, start_time, end_time FROM transfer_balance WHERE network_id = $1 ORDER BY start_time, grant_kind`,
			networkId,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				balance := &testTeamBalance{}
				server.Raise(rows.Scan(&balance.grantKind, &balance.tier, &balance.pro, &balance.startTime, &balance.endTime))
				balance.startTime = balance.startTime.UTC()
				balance.endTime = balance.endTime.UTC()
				balances = append(balances, balance)
			}
		})
	})
	return
}

// The network's balance of the kind that starts at startTime, nil when there is none.
func testTeamBalanceAt(balances []*testTeamBalance, grantKind string, startTime time.Time) *testTeamBalance {
	for _, balance := range balances {
		if balance.grantKind == grantKind && balance.startTime.Equal(startTime) {
			return balance
		}
	}
	return nil
}

// The network's renewals of the type, as (start, end, revenue) rows by start.
type testTeamRenewal struct {
	startTime     time.Time
	endTime       time.Time
	netRevenue    model.NanoCents
	transactionId string
}

func testTeamRenewals(t testing.TB, ctx context.Context, networkId server.Id, subscriptionType string) (renewals []*testTeamRenewal) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT start_time, end_time, net_revenue_nano_cents, COALESCE(transaction_id, '') FROM subscription_renewal WHERE network_id = $1 AND subscription_type = $2 ORDER BY start_time`,
			networkId,
			subscriptionType,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				renewal := &testTeamRenewal{}
				server.Raise(rows.Scan(&renewal.startTime, &renewal.endTime, &renewal.netRevenue, &renewal.transactionId))
				renewal.startTime = renewal.startTime.UTC()
				renewal.endTime = renewal.endTime.UTC()
				renewals = append(renewals, renewal)
			}
		})
	})
	return
}

// Sets the clock the Stripe handlers read until the returned restore function runs.
func testSetStripeDunningNow(now time.Time) func() {
	previous := stripeDunningNow
	stripeDunningNow = func() time.Time { return now }
	return func() { stripeDunningNow = previous }
}
