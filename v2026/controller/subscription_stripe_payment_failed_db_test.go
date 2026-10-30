package controller

// Root-cause tests of the failed-payment follow-up (owner request 2026-10-26:
// declined renewals must ask the customer to update their payment method).
// Before the follow-up, invoice.payment_failed fell through StripeWebhook to
// "ignore and answer 200": no email reached an account whose Stripe customer
// has no email, nothing told the apps the renewal was failing, and the billing
// portal answered "No stripe customer found" to every network that subscribed
// through Checkout, which writes no stripe_customer row. These tests drive the
// public entry points (the webhook, the outbox delivery, the portal and the
// balance) against a fake Stripe API, so they read the same before and after
// the change. Needs the test database and redis (server.DefaultTestEnv).

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

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The template name of the payment-failed notice, as a string, so these tests
// do not depend on the template type.
const stripePaymentFailedTestTemplateName = "subscription_payment_failed"

// A fake Stripe API: invoices, subscriptions and checkout sessions by id, read
// through the raw-HTTP seam and the stripe-go backend alike, and billing portal
// sessions, whose forms it records. Invoices are always served expanded (their
// subscription and customer as objects), which every reader of an invoice
// accepts. Safe for concurrent use.
type stripePaymentFailedTestEnv struct {
	stateLock              sync.Mutex
	invoices               map[string]map[string]any
	subscriptions          map[string]map[string]any
	sessionsBySubscription map[string][]map[string]any
	portalForms            []url.Values
	// refuse a portal session that asks for a flow
	refusePortalFlow bool

	testServer *httptest.Server
}

func newStripePaymentFailedTestEnv(t testing.TB) *stripePaymentFailedTestEnv {
	env := &stripePaymentFailedTestEnv{
		invoices:               map[string]map[string]any{},
		subscriptions:          map[string]map[string]any{},
		sessionsBySubscription: map[string][]map[string]any{},
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
	mux.HandleFunc("GET /v1/invoices/{invoiceId}", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		invoice, ok := env.invoices[r.PathValue("invoiceId")]
		env.stateLock.Unlock()
		if !ok {
			notFound(w)
			return
		}
		writeJson(w, http.StatusOK, invoice)
	})
	mux.HandleFunc("GET /v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		sub, ok := env.subscriptions[r.PathValue("subscriptionId")]
		env.stateLock.Unlock()
		if !ok {
			notFound(w)
			return
		}
		writeJson(w, http.StatusOK, sub)
	})
	mux.HandleFunc("GET /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		env.stateLock.Lock()
		subs := []map[string]any{}
		for _, sub := range env.subscriptions {
			if sub["status"] == status {
				subs = append(subs, sub)
			}
		}
		env.stateLock.Unlock()
		writeJson(w, http.StatusOK, map[string]any{"object": "list", "data": subs, "has_more": false})
	})
	mux.HandleFunc("GET /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		env.stateLock.Lock()
		sessions := env.sessionsBySubscription[r.URL.Query().Get("subscription")]
		env.stateLock.Unlock()
		if sessions == nil {
			sessions = []map[string]any{}
		}
		writeJson(w, http.StatusOK, map[string]any{"data": sessions, "has_more": false})
	})
	mux.HandleFunc("POST /v1/billing_portal/sessions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			writeJson(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "bad form"}})
			return
		}
		env.stateLock.Lock()
		env.portalForms = append(env.portalForms, form)
		refuseFlow := env.refusePortalFlow
		env.stateLock.Unlock()
		if refuseFlow && form.Get("flow_data[type]") != "" {
			writeJson(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "the portal configuration does not allow this flow"}})
			return
		}
		writeJson(w, http.StatusOK, map[string]any{
			"id":       "bps_synthetic",
			"object":   "billing_portal.session",
			"customer": form.Get("customer"),
			"url":      "https://billing.stripe.example/p/session/" + form.Get("customer"),
		})
	})
	env.testServer = httptest.NewServer(mux)

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousToken := stripeApiToken
	previousBackend := stripe.GetBackend(stripe.APIBackend)
	stripeApiBaseUrl = env.testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_payment_failed" }
	stripeApiToken = func() string { return "sk_test_payment_failed" }
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(env.testServer.URL),
		MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		stripeApiToken = previousToken
		stripe.SetBackend(stripe.APIBackend, previousBackend)
		env.testServer.Close()
	})
	return env
}

// Sets a subscription Stripe bills the network through, in `status`.
func (self *stripePaymentFailedTestEnv) setSubscription(subscriptionId string, customerId string, networkId *server.Id, status string) {
	metadata := map[string]any{}
	if networkId != nil {
		metadata["network_id"] = networkId.String()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.subscriptions[subscriptionId] = map[string]any{
		"id":       subscriptionId,
		"object":   "subscription",
		"status":   status,
		"customer": customerId,
		"metadata": metadata,
		"items":    map[string]any{"object": "list", "data": []any{}},
	}
}

// Sets the subscription's renewal invoice as Stripe reports it now: its status,
// its failed attempts, and the next attempt (nil: Stripe will not retry).
func (self *stripePaymentFailedTestEnv) setInvoice(
	invoiceId string,
	subscriptionId string,
	customerId string,
	status string,
	attemptCount int,
	created time.Time,
	next *time.Time,
	amountDue int64,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var nextPaymentAttempt any
	if next != nil {
		nextPaymentAttempt = next.Unix()
	}
	var subMetadata any = map[string]any{}
	if sub, ok := self.subscriptions[subscriptionId]; ok {
		subMetadata = sub["metadata"]
	}
	self.invoices[invoiceId] = map[string]any{
		"id":                   invoiceId,
		"object":               "invoice",
		"status":               status,
		"paid":                 status == "paid",
		"billing_reason":       "subscription_cycle",
		"attempt_count":        attemptCount,
		"next_payment_attempt": nextPaymentAttempt,
		"hosted_invoice_url":   "https://invoice.stripe.com/i/acct_synthetic/" + invoiceId,
		"amount_due":           amountDue,
		"total":                amountDue,
		"currency":             "usd",
		"created":              created.Unix(),
		"customer":             map[string]any{"id": customerId, "object": "customer", "email": ""},
		"subscription":         map[string]any{"id": subscriptionId, "object": "subscription", "metadata": subMetadata},
		"lines": map[string]any{"object": "list", "data": []any{map[string]any{
			"id":           "il_" + invoiceId,
			"type":         "subscription",
			"subscription": subscriptionId,
			"amount_total": amountDue,
			"currency":     "usd",
			"quantity":     1,
			"period":       map[string]any{"start": created.Unix(), "end": created.Add(365 * 24 * time.Hour).Unix()},
		}}},
	}
}

// Sets the subscription's latest invoice, as a status listing reports it.
func (self *stripePaymentFailedTestEnv) setLatestInvoice(subscriptionId string, invoiceId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.subscriptions[subscriptionId]["latest_invoice"] = invoiceId
}

// Whether a portal session that asks for a flow is refused.
func (self *stripePaymentFailedTestEnv) setRefusePortalFlow(refusePortalFlow bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusePortalFlow = refusePortalFlow
}

// The billing portal forms posted so far.
func (self *stripePaymentFailedTestEnv) postedPortalForms() []url.Values {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]url.Values{}, self.portalForms...)
}

// A network whose admin signs in with `email`; "" leaves the account without an
// address (a guest network stands for a wallet or seedphrase account).
func newStripePaymentFailedTestNetwork(t testing.TB, ctx context.Context, name string, email string) (server.Id, *session.ClientSession) {
	networkId := server.NewId()
	userId := server.NewId()
	if email == "" {
		model.Testing_CreateGuestNetwork(ctx, networkId, name, userId)
	} else {
		model.Testing_CreateNetwork(ctx, networkId, name, userId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`UPDATE network_user SET user_auth = $1 WHERE user_id = $2`,
				email, userId))
		})
	}
	clientId := server.NewId()
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		ClientId:  &clientId,
		UserId:    userId,
	})
	return networkId, clientSession
}

// Delivers the invoice.payment_failed event of the invoice, as Stripe would.
func deliverStripePaymentFailedTestEvent(t testing.TB, clientSession *session.ClientSession, invoiceId string) {
	result, err := StripeWebhook(stripeWebhookEvent(t, "invoice.payment_failed", map[string]any{
		"id":             invoiceId,
		"object":         "invoice",
		"billing_reason": "subscription_cycle",
		"status":         "open",
	}), clientSession)
	if err != nil || result == nil {
		t.Fatalf("invoice.payment_failed for %s: %+v %v", invoiceId, result, err)
	}
}

// Runs the outbox delivery and returns the payment-failed notices sent so far
// to `email`.
func deliverStripePaymentFailedTestNotices(ctx context.Context, sender *outboxTestSender, email string) []outboxTestSend {
	deliverAccountMessagesAt(ctx, sender, server.NowUtc().Add(time.Minute))
	notices := []outboxTestSend{}
	for _, send := range sender.sent() {
		if send.template.Name() == stripePaymentFailedTestTemplateName && send.userAuth == email {
			notices = append(notices, send)
		}
	}
	return notices
}

// The first failed renewal emails the account once, however often Stripe
// delivers the event. Before the follow-up the event was ignored and nothing
// was sent.
func TestStripeRenewalPaymentFailedEmailsTheAccountOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "payment-failed@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "paymentfailedonce", email)

		created := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		next := created.Add(2 * 24 * time.Hour)
		env.setSubscription("sub_synthetic_once", "cus_synthetic_once", &networkId, "past_due")
		env.setInvoice("in_synthetic_once", "sub_synthetic_once", "cus_synthetic_once", "open", 1, created, &next, 4000)

		sender := newOutboxTestSender()
		deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_once")
		notices := deliverStripePaymentFailedTestNotices(ctx, sender, email)
		if len(notices) != 1 {
			t.Fatalf("the first failure must email the account once, got %d notices", len(notices))
		}

		// Stripe delivers at least once, and the dashboard can resend
		for range 3 {
			deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_once")
		}
		if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 1 {
			t.Fatalf("a redelivered failure must not email again, got %d notices", len(notices))
		}
	})
}

// A renewal that fails through all its retries emails twice: at the first
// failure, and once more before the subscription ends. Nothing in between, and
// nothing after.
func TestStripeRenewalPaymentFailedEmailsAtFirstAndFinalAttemptOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "payment-failed-final@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "paymentfailedfinal", email)

		day := 24 * time.Hour
		created := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		env.setSubscription("sub_synthetic_final", "cus_synthetic_final", &networkId, "past_due")
		sender := newOutboxTestSender()
		// Smart Retries on the account: 8 retries within 2 weeks
		nextAttemptOffsets := []time.Duration{1 * day, 3 * day, 5 * day, 6 * day, 7 * day, 8 * day, 9 * day, 14 * day}
		wantNotices := []int{1, 1, 1, 1, 1, 1, 1, 2, 2}
		for attemptCount := 1; attemptCount <= 9; attemptCount += 1 {
			var next *time.Time
			if attemptCount <= len(nextAttemptOffsets) {
				nextAttempt := created.Add(nextAttemptOffsets[attemptCount-1])
				next = &nextAttempt
			}
			env.setInvoice("in_synthetic_final", "sub_synthetic_final", "cus_synthetic_final", "open", attemptCount, created, next, 500)
			deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_final")
			if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != wantNotices[attemptCount-1] {
				t.Fatalf("after failed attempt %d: %d notices, want %d", attemptCount, len(notices), wantNotices[attemptCount-1])
			}
		}
		// the events redelivered after the final notice
		for range 2 {
			deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_final")
		}
		notices := deliverStripePaymentFailedTestNotices(ctx, sender, email)
		if len(notices) != 2 {
			t.Fatalf("an invoice gets at most two notices, got %d", len(notices))
		}
		subjects := []string{}
		for _, notice := range notices {
			subject, _, _, err := RenderEmailTemplate(notice.template)
			if err != nil {
				t.Fatal(err)
			}
			subjects = append(subjects, subject)
		}
		if subjects[0] != "Your URnetwork payment didn't go through" || !strings.HasPrefix(subjects[1], "Final notice:") {
			t.Fatalf("the first then the final notice, got %q", subjects)
		}
	})
}

// A failure whose invoice Stripe now reports paid, voided or uncollectible, or
// whose subscription already ended, owes nothing: the event can arrive late.
func TestStripeRenewalPaymentFailedNoEmailForPaidOrVoidedInvoice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		const email = "payment-failed-paid@synthetic.example"
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "paymentfailedpaid", email)

		created := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		next := created.Add(2 * 24 * time.Hour)
		sender := newOutboxTestSender()
		for i, state := range []struct {
			invoiceStatus      string
			subscriptionStatus string
		}{
			{"paid", "active"},
			{"void", "past_due"},
			{"uncollectible", "past_due"},
			{"open", "canceled"},
		} {
			subscriptionId := fmt.Sprintf("sub_synthetic_paid_%d", i)
			invoiceId := fmt.Sprintf("in_synthetic_paid_%d", i)
			env.setSubscription(subscriptionId, "cus_synthetic_paid", &networkId, state.subscriptionStatus)
			env.setInvoice(invoiceId, subscriptionId, "cus_synthetic_paid", state.invoiceStatus, 1, created, &next, 4000)
			deliverStripePaymentFailedTestEvent(t, clientSession, invoiceId)
			if notices := deliverStripePaymentFailedTestNotices(ctx, sender, email); len(notices) != 0 {
				t.Fatalf("invoice %s, subscription %s: %d notices, want none", state.invoiceStatus, state.subscriptionStatus, len(notices))
			}
		}
	})
}

// An account without an email (a wallet or seedphrase account) gets no email,
// and its apps show the past-due notice from the balance until the failure is
// resolved.
func TestStripeRenewalPaymentFailedShowsPastDueInTheBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		networkId, clientSession := newStripePaymentFailedTestNetwork(t, ctx, "paymentfailedwallet", "")

		created := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		next := created.Add(2 * 24 * time.Hour)
		env.setSubscription("sub_synthetic_wallet", "cus_synthetic_wallet", &networkId, "past_due")
		env.setInvoice("in_synthetic_wallet", "sub_synthetic_wallet", "cus_synthetic_wallet", "open", 1, created, &next, 4000)

		pastDue := func() map[string]any {
			balance, err := SubscriptionBalance(clientSession)
			if err != nil {
				t.Fatal(err)
			}
			balanceJson, err := json.Marshal(balance)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(balanceJson, &fields); err != nil {
				t.Fatal(err)
			}
			notice, _ := fields["payment_past_due"].(map[string]any)
			return notice
		}
		if notice := pastDue(); notice != nil {
			t.Fatalf("no failure yet, got a past-due notice %v", notice)
		}

		sender := newOutboxTestSender()
		deliverStripePaymentFailedTestEvent(t, clientSession, "in_synthetic_wallet")
		deliverAccountMessagesAt(ctx, sender, server.NowUtc().Add(time.Minute))
		for _, send := range sender.sent() {
			if send.template.Name() == stripePaymentFailedTestTemplateName {
				t.Fatalf("an account without an email got a notice sent to %q", send.userAuth)
			}
		}
		notice := pastDue()
		if notice == nil || notice["store"] != "stripe" || notice["update_payment_url"] != "https://ur.io/app/subscription" {
			t.Fatalf("the failing renewal must show the past-due notice, got %v", notice)
		}

		// voided: the notice ends
		result, err := StripeWebhook(stripeWebhookEvent(t, "invoice.voided", map[string]any{
			"id":     "in_synthetic_wallet",
			"object": "invoice",
			"status": "void",
		}), clientSession)
		if err != nil || result == nil {
			t.Fatalf("invoice.voided: %+v %v", result, err)
		}
		if notice := pastDue(); notice != nil {
			t.Fatalf("a voided invoice must end the past-due notice, got %v", notice)
		}
	})
}

// The billing portal opens for the caller's own Stripe customer only: the one
// the server made for the network, or the customer of the network's own Stripe
// renewal when it subscribed through Checkout (no stripe_customer row). A
// network with neither gets no portal, and no session is made for it.
func TestStripeCustomerPortalOpensOnlyTheCallersOwnCustomer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newStripePaymentFailedTestEnv(t)
		now := server.NowUtc().Truncate(time.Second)

		// a network the payment sheet made a customer for
		_, sheetSession := newStripePaymentFailedTestNetwork(t, ctx, "portalsheet", "portal-sheet@synthetic.example")
		if err := model.CreateStripeCustomer("cus_synthetic_sheet", sheetSession); err != nil {
			t.Fatal(err)
		}

		// a network that subscribed through Checkout: a credited Stripe renewal,
		// and no stripe_customer row
		checkoutNetworkId, checkoutSession := newStripePaymentFailedTestNetwork(t, ctx, "portalcheckout", "portal-checkout@synthetic.example")
		env.setSubscription("sub_synthetic_checkout", "cus_synthetic_checkout", &checkoutNetworkId, "past_due")
		env.setInvoice("in_synthetic_checkout", "sub_synthetic_checkout", "cus_synthetic_checkout", "paid", 1, now.Add(-300*24*time.Hour), nil, 4000)
		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          checkoutNetworkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-300 * 24 * time.Hour),
			EndTime:            now.Add(95 * 24 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(28),
			SubscriptionMarket: model.SubscriptionMarketStripe,
			TransactionId:      "in_synthetic_checkout",
		}); err != nil {
			t.Fatal(err)
		}

		// a network Stripe never billed
		_, otherSession := newStripePaymentFailedTestNetwork(t, ctx, "portalother", "portal-other@synthetic.example")

		for _, c := range []struct {
			clientSession *session.ClientSession
			customerId    string
		}{
			{sheetSession, "cus_synthetic_sheet"},
			{checkoutSession, "cus_synthetic_checkout"},
		} {
			portal, err := StripeCreateCustomerPortal(&StripeCreateCustomerPortalArgs{}, c.clientSession)
			if err != nil || portal.Error != nil || portal.Url != "https://billing.stripe.example/p/session/"+c.customerId {
				t.Fatalf("portal for %s: %+v %v", c.customerId, portal, err)
			}
		}
		formCount := len(env.postedPortalForms())
		portal, err := StripeCreateCustomerPortal(&StripeCreateCustomerPortalArgs{}, otherSession)
		if err != nil || portal.Url != "" || portal.Error == nil || portal.Error.Code != SubscriptionErrorCodeNoCustomer {
			t.Fatalf("a network without a customer must get no portal: %+v %v", portal, err)
		}
		forms := env.postedPortalForms()
		if len(forms) != formCount || len(forms) != 2 {
			t.Fatalf("expected exactly the two callers' sessions, got %d forms", len(forms))
		}
		for i, customerId := range []string{"cus_synthetic_sheet", "cus_synthetic_checkout"} {
			if forms[i].Get("customer") != customerId {
				t.Fatalf("session %d was made for %q, want the caller's own %q", i, forms[i].Get("customer"), customerId)
			}
		}
	})
}
