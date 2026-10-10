package controller

// Hermetic tests for the "Manage subscription" details: the per-store
// collapse, the store-state mappings and the cancel refusals. The store APIs
// are either the lookup seam (buildSubscriptionDetails) or a fake httptest
// Stripe behind the same base-url/token seams the webhooks use. The
// database-backed walk-through is in subscription_details_db_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func subscriptionTestRenewal(market string, start time.Time, end time.Time, transactionId string, purchaseToken string) *model.ActiveSubscriptionRenewal {
	return &model.ActiveSubscriptionRenewal{
		Market:        market,
		StartTime:     start,
		EndTime:       end,
		TransactionId: transactionId,
		PurchaseToken: purchaseToken,
	}
}

func TestSubscriptionDetailsCollapsesPerStore(t *testing.T) {
	now := server.NowUtc()
	renewals := []*model.ActiveSubscriptionRenewal{
		// two stripe windows: the older one ends first, the newer one is what
		// the customer is paid through
		subscriptionTestRenewal(model.SubscriptionMarketStripe, now.Add(-40*24*time.Hour), now.Add(2*24*time.Hour), "in_old", ""),
		subscriptionTestRenewal(model.SubscriptionMarketStripe, now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), "in_new", ""),
		// a one-time yearly usdc payment
		subscriptionTestRenewal(model.SubscriptionMarketSolana, now.Add(-30*24*time.Hour), now.Add(335*24*time.Hour), "ref_1", ""),
	}

	stripeCalls := 0
	lookups := &subscriptionStoreLookupSet{
		stripe: func(ctx context.Context, customerId string, invoiceId string) (*subscriptionStoreState, error) {
			stripeCalls += 1
			if customerId != "cus_1" || invoiceId != "in_new" {
				t.Fatalf("stripe lookup got customer %q invoice %q", customerId, invoiceId)
			}
			end := now.Add(21 * 24 * time.Hour)
			return &subscriptionStoreState{
				AutoRenew:           boolPtr(true),
				EndTime:             &end,
				Cadence:             SubscriptionCadenceMonthly,
				StoreSubscriptionId: "sub_1",
				Active:              true,
			}, nil
		},
		solana: solanaLookupSubscriptionStateForTest(SubscriptionCadenceYearly),
	}

	// the network's last stripe window is the active one: stripe is asked once
	result := buildSubscriptionDetails(context.Background(), renewals, renewals[1], "cus_1", lookups)

	if !result.HasStripeCustomer {
		t.Fatal("expected has_stripe_customer")
	}
	if len(result.Subscriptions) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(result.Subscriptions))
	}
	if stripeCalls != 1 {
		t.Fatalf("expected one stripe lookup for the collapsed entry, got %d", stripeCalls)
	}

	solana := result.Subscriptions[0]
	stripe := result.Subscriptions[1]
	if solana.Store != model.SubscriptionMarketSolana || stripe.Store != model.SubscriptionMarketStripe {
		t.Fatalf("expected [solana, stripe] (sorted by store), got [%s, %s]", solana.Store, stripe.Store)
	}

	if stripe.AutoRenew == nil || !*stripe.AutoRenew {
		t.Fatal("stripe: expected auto_renew true from the store")
	}
	if !stripe.CanCancel || stripe.ManageUrl != "" {
		t.Fatal("stripe: expected can_cancel and no manage url")
	}
	if !stripe.EndTime.Equal(now.Add(21 * 24 * time.Hour)) {
		t.Fatalf("stripe: expected the store's period end, got %v", stripe.EndTime)
	}
	if stripe.Cadence != SubscriptionCadenceMonthly || stripe.TransactionId != "in_new" {
		t.Fatalf("stripe: cadence %q transaction %q", stripe.Cadence, stripe.TransactionId)
	}
	if stripe.Plan != model.SubscriptionTypeSupporter {
		t.Fatalf("stripe: plan %q", stripe.Plan)
	}

	if solana.AutoRenew == nil || *solana.AutoRenew {
		t.Fatal("solana: a usdc payment never renews")
	}
	if solana.CanCancel || solana.ManageUrl != "" || solana.CancelAtPeriodEnd {
		t.Fatal("solana: nothing to cancel")
	}
	if solana.Cadence != SubscriptionCadenceYearly {
		t.Fatalf("solana: cadence %q", solana.Cadence)
	}
	if !solana.EndTime.Equal(now.Add(335 * 24 * time.Hour)) {
		t.Fatalf("solana: expected the row's window end, got %v", solana.EndTime)
	}

	// the wire shape: auto_renew is a real null when unknown, never omitted
	raw, _ := json.Marshal(&SubscriptionDetail{Store: "stripe"})
	if string(raw) == "" || !containsJsonKey(raw, "auto_renew") {
		t.Fatalf("auto_renew must serialize even when unknown: %s", raw)
	}
}

func solanaLookupSubscriptionStateForTest(cadence string) func(ctx context.Context, reference string) (*subscriptionStoreState, error) {
	return func(ctx context.Context, reference string) (*subscriptionStoreState, error) {
		return &subscriptionStoreState{AutoRenew: boolPtr(false), Cadence: cadence}, nil
	}
}

func containsJsonKey(raw []byte, key string) bool {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func TestSubscriptionDetailsLookupFailureIsUnknownNotError(t *testing.T) {
	now := server.NowUtc()
	renewals := []*model.ActiveSubscriptionRenewal{
		subscriptionTestRenewal(model.SubscriptionMarketStripe, now.Add(-1*24*time.Hour), now.Add(29*24*time.Hour), "in_1", ""),
		subscriptionTestRenewal(model.SubscriptionMarketApple, now.Add(-1*24*time.Hour), now.Add(364*24*time.Hour), "2000000123", ""),
		subscriptionTestRenewal(model.SubscriptionMarketGoogle, now.Add(-1*24*time.Hour), now.Add(29*24*time.Hour), "", "tok_1"),
		// a manual grant: no store to ask
		subscriptionTestRenewal(model.SubscriptionMarketManual, now.Add(-1*24*time.Hour), now.Add(100*24*time.Hour), "", ""),
	}
	lookups := &subscriptionStoreLookupSet{
		stripe: func(ctx context.Context, customerId string, invoiceId string) (*subscriptionStoreState, error) {
			return nil, errors.New("stripe is down")
		},
		apple: func(ctx context.Context, originalTransactionId string) (*subscriptionStoreState, error) {
			return nil, errors.New("apple is down")
		},
		google: func(ctx context.Context, purchaseToken string) (*subscriptionStoreState, error) {
			return nil, errors.New("play is down")
		},
	}

	result := buildSubscriptionDetails(context.Background(), renewals, renewals[0], "", lookups)
	if result.HasStripeCustomer {
		t.Fatal("no customer row -> has_stripe_customer false")
	}
	byStore := map[string]*SubscriptionDetail{}
	for _, detail := range result.Subscriptions {
		byStore[detail.Store] = detail
	}
	if len(byStore) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(byStore))
	}
	for _, store := range []string{model.SubscriptionMarketStripe, model.SubscriptionMarketApple, model.SubscriptionMarketGoogle} {
		detail := byStore[store]
		if detail.AutoRenew != nil {
			t.Fatalf("%s: a failed lookup is unknown, got %v", store, *detail.AutoRenew)
		}
	}
	// the row's own window still answers "until when"
	if !byStore[model.SubscriptionMarketApple].EndTime.Equal(now.Add(364 * 24 * time.Hour)) {
		t.Fatal("apple: expected the row's end time")
	}
	// and the window length still names the cadence
	if byStore[model.SubscriptionMarketApple].Cadence != SubscriptionCadenceYearly {
		t.Fatalf("apple: cadence %q", byStore[model.SubscriptionMarketApple].Cadence)
	}
	if byStore[model.SubscriptionMarketStripe].Cadence != SubscriptionCadenceMonthly {
		t.Fatalf("stripe: cadence %q", byStore[model.SubscriptionMarketStripe].Cadence)
	}
	if byStore[model.SubscriptionMarketApple].ManageUrl != appleManageSubscriptionsUrl ||
		byStore[model.SubscriptionMarketGoogle].ManageUrl != googleManageSubscriptionsUrl {
		t.Fatal("apple/google: expected the store subscriptions pages")
	}
	if byStore[model.SubscriptionMarketGoogle].TransactionId != "tok_1" {
		t.Fatal("google: the purchase token is the handle")
	}
	manual := byStore[model.SubscriptionMarketManual]
	if manual.AutoRenew == nil || *manual.AutoRenew || manual.CanCancel || manual.ManageUrl != "" {
		t.Fatal("manual: known not to renew, nothing to cancel")
	}
	// 100 days: neither band
	if manual.Cadence != "" {
		t.Fatalf("manual: cadence %q", manual.Cadence)
	}
}

// A Stripe renewal whose payment fails is retried (past_due) after the last
// window it paid for has ended (support inbox item 7873): no active row names
// Stripe, yet the card is still charged. The last window's invoice is the
// handle (a web checkout network has no customer on file), and Stripe's
// answer that it still bills puts the subscription on the screen, cancellable,
// beside the stores that do have an active window.
func TestSubscriptionDetailsListsAStripeRenewalStillBeingRetried(t *testing.T) {
	now := server.NowUtc()
	renewals := []*model.ActiveSubscriptionRenewal{
		subscriptionTestRenewal(model.SubscriptionMarketApple, now.Add(-1*24*time.Hour), now.Add(29*24*time.Hour), "2000000777", ""),
	}
	lastStripeRenewal := subscriptionTestRenewal(model.SubscriptionMarketStripe, now.Add(-370*24*time.Hour), now.Add(-5*24*time.Hour), "in_paid", "")
	periodEnd := now.Add(360 * 24 * time.Hour)

	stripeCalls := 0
	lookups := &subscriptionStoreLookupSet{
		stripe: func(ctx context.Context, customerId string, invoiceId string) (*subscriptionStoreState, error) {
			stripeCalls += 1
			if customerId != "" || invoiceId != "in_paid" {
				t.Fatalf("stripe lookup got customer %q invoice %q", customerId, invoiceId)
			}
			// past_due: still billing, still set to renew
			return &subscriptionStoreState{
				AutoRenew:           boolPtr(true),
				EndTime:             &periodEnd,
				Cadence:             SubscriptionCadenceYearly,
				StoreSubscriptionId: "sub_retry",
				Active:              true,
			}, nil
		},
		apple: func(ctx context.Context, originalTransactionId string) (*subscriptionStoreState, error) {
			return nil, errors.New("apple is not part of this test")
		},
	}

	result := buildSubscriptionDetails(context.Background(), renewals, lastStripeRenewal, "", lookups)
	if stripeCalls != 1 {
		t.Fatalf("expected one stripe lookup, got %d", stripeCalls)
	}
	if result.HasStripeCustomer {
		t.Fatal("no customer row -> has_stripe_customer false")
	}
	if len(result.Subscriptions) != 2 {
		t.Fatalf("expected the apple entry and the stripe one still billing, got %d: %+v", len(result.Subscriptions), result.Subscriptions)
	}
	apple, stripe := result.Subscriptions[0], result.Subscriptions[1]
	if apple.Store != model.SubscriptionMarketApple || stripe.Store != model.SubscriptionMarketStripe {
		t.Fatalf("expected [apple, stripe] (sorted by store), got [%s, %s]", apple.Store, stripe.Store)
	}
	if !stripe.CanCancel || stripe.ManageUrl != "" || stripe.TransactionId != "in_paid" {
		t.Fatalf("stripe: cancellable here, by the last window's invoice: %+v", stripe)
	}
	if stripe.AutoRenew == nil || !*stripe.AutoRenew || stripe.CancelAtPeriodEnd {
		t.Fatalf("stripe: the store's renewal state: %+v", stripe)
	}
	if !stripe.StartTime.Equal(lastStripeRenewal.StartTime) || !stripe.EndTime.Equal(periodEnd) || stripe.Cadence != SubscriptionCadenceYearly {
		t.Fatalf("stripe: the last window's start and the store's period end: %+v", stripe)
	}
}

// Behind an old Stripe window the subscription is usually over. Only Stripe's
// answer that it still bills lists it: an ended subscription, a lookup that
// failed and a lookup with nothing to say all leave the screen without it.
func TestSubscriptionDetailsLeavesAnEndedStripeWindowUnlisted(t *testing.T) {
	now := server.NowUtc()
	lastStripeRenewal := subscriptionTestRenewal(model.SubscriptionMarketStripe, now.Add(-36*24*time.Hour), now.Add(-5*24*time.Hour), "in_old", "")
	cases := []struct {
		name  string
		state *subscriptionStoreState
		err   error
	}{
		{name: "canceled", state: stripeSubscriptionState(&stripeCustomerSubscription{Id: "sub_over", Status: "canceled"})},
		{name: "unpaid", state: stripeSubscriptionState(&stripeCustomerSubscription{Id: "sub_unpaid", Status: "unpaid"})},
		{name: "incomplete_expired", state: stripeSubscriptionState(&stripeCustomerSubscription{Id: "sub_expired", Status: "incomplete_expired"})},
		{name: "lookup failed", err: errors.New("stripe is down")},
		{name: "nothing to say"},
	}
	for _, c := range cases {
		stripeCalls := 0
		lookups := &subscriptionStoreLookupSet{
			stripe: func(ctx context.Context, customerId string, invoiceId string) (*subscriptionStoreState, error) {
				stripeCalls += 1
				if customerId != "cus_1" || invoiceId != "in_old" {
					t.Errorf("%s: stripe lookup got customer %q invoice %q", c.name, customerId, invoiceId)
				}
				return c.state, c.err
			},
		}
		result := buildSubscriptionDetails(context.Background(), nil, lastStripeRenewal, "cus_1", lookups)
		if stripeCalls != 1 {
			t.Errorf("%s: expected one stripe lookup, got %d", c.name, stripeCalls)
		}
		if len(result.Subscriptions) != 0 {
			t.Errorf("%s: nothing bills the network, got %+v", c.name, result.Subscriptions)
		}
		if !result.HasStripeCustomer {
			t.Errorf("%s: the customer row still opens the billing portal", c.name)
		}
	}

	// a network that never had a stripe window is not asked about one
	result := buildSubscriptionDetails(context.Background(), nil, nil, "", &subscriptionStoreLookupSet{
		stripe: func(ctx context.Context, customerId string, invoiceId string) (*subscriptionStoreState, error) {
			t.Fatal("no stripe window to ask about")
			return nil, nil
		},
	})
	if len(result.Subscriptions) != 0 {
		t.Fatalf("nothing billing: %+v", result.Subscriptions)
	}
}

func TestSubscriptionCadenceFromWindow(t *testing.T) {
	now := server.NowUtc()
	cases := []struct {
		days     float64
		expected string
	}{
		{30, SubscriptionCadenceMonthly},
		{31, SubscriptionCadenceMonthly},
		{45, SubscriptionCadenceMonthly}, // monthly + stripe grace
		{62, SubscriptionCadenceMonthly},
		{100, ""},
		{180, SubscriptionCadenceYearly},
		{365, SubscriptionCadenceYearly},
		{380, SubscriptionCadenceYearly}, // yearly + trial + grace
		{0, ""},
	}
	for _, c := range cases {
		got := subscriptionCadenceFromWindow(now, now.Add(time.Duration(c.days*24)*time.Hour))
		if got != c.expected {
			t.Fatalf("%v days: expected %q, got %q", c.days, c.expected, got)
		}
	}
	if subscriptionCadenceFromProductId("supporter_monthly_26") != SubscriptionCadenceMonthly ||
		subscriptionCadenceFromProductId("pro_yearly") != SubscriptionCadenceYearly ||
		subscriptionCadenceFromProductId("supporter_annual") != SubscriptionCadenceYearly ||
		subscriptionCadenceFromProductId("supporter") != "" {
		t.Fatal("product id cadence")
	}
}

func TestStripeSubscriptionState(t *testing.T) {
	end := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	raw := `{
		"id": "sub_1", "status": "active", "cancel_at_period_end": false, "created": 1700000000,
		"items": {"data": [{"current_period_end": ` + itoa(end.Unix()) + `, "price": {"id": "price_x", "recurring": {"interval": "year"}}}]}
	}`
	sub := &stripeCustomerSubscription{}
	if err := json.Unmarshal([]byte(raw), sub); err != nil {
		t.Fatal(err)
	}
	state := stripeSubscriptionState(sub)
	if state.AutoRenew == nil || !*state.AutoRenew || state.CancelAtPeriodEnd || !state.Active {
		t.Fatal("active + not cancelling = renews")
	}
	if state.EndTime == nil || !state.EndTime.Equal(end) {
		t.Fatalf("period end from the items (API 2025-03-31+): %v", state.EndTime)
	}
	if state.Cadence != SubscriptionCadenceYearly || state.StoreSubscriptionId != "sub_1" {
		t.Fatalf("cadence %q id %q", state.Cadence, state.StoreSubscriptionId)
	}

	// cancel at period end: still active, paid through, will not renew
	sub.CancelAtPeriodEnd = true
	state = stripeSubscriptionState(sub)
	if state.AutoRenew == nil || *state.AutoRenew || !state.CancelAtPeriodEnd || !state.Active {
		t.Fatal("cancel_at_period_end: auto_renew false, flag set, still active")
	}

	// stripe already ended it
	sub.Status = "canceled"
	sub.CancelAtPeriodEnd = false
	state = stripeSubscriptionState(sub)
	if state.AutoRenew == nil || *state.AutoRenew || state.Active || state.CancelAtPeriodEnd {
		t.Fatal("canceled: over")
	}

	// the older shape: current_period_end on the subscription, interval month
	sub = &stripeCustomerSubscription{Id: "sub_2", Status: "trialing", CurrentPeriodEnd: end.Unix()}
	sub.Items.Data = append(sub.Items.Data, struct {
		CurrentPeriodEnd int64 `json:"current_period_end"`
		Price            struct {
			Id        string `json:"id"`
			Recurring *struct {
				Interval string `json:"interval"`
			} `json:"recurring"`
		} `json:"price"`
	}{})
	sub.Items.Data[0].Price.Id = "price_pro_monthly"
	state = stripeSubscriptionState(sub)
	if state.AutoRenew == nil || !*state.AutoRenew || state.EndTime == nil || !state.EndTime.Equal(end) {
		t.Fatal("trialing renews; subscription-level period end")
	}
	if state.Cadence != SubscriptionCadenceMonthly {
		t.Fatalf("cadence from the price id: %q", state.Cadence)
	}
}

func itoa(v int64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestAppleSubscriptionState(t *testing.T) {
	expires := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	statuses := &appleSubscriptionStatusesResponse{
		Data: []*appleSubscriptionGroup{{
			LastTransactions: []*appleLastTransaction{{
				Status:                1,
				OriginalTransactionId: "2000000123",
				SignedTransactionInfo: appleTestJws(t, map[string]any{
					"productId":   "supporter_yearly_26",
					"expiresDate": float64(expires.UnixMilli()),
				}),
				SignedRenewalInfo: appleTestJws(t, map[string]any{
					"autoRenewStatus":    float64(0),
					"autoRenewProductId": "supporter_yearly_26",
				}),
			}},
		}},
	}
	state := appleSubscriptionState(statuses)
	if state == nil || state.AutoRenew == nil || *state.AutoRenew || !state.CancelAtPeriodEnd || !state.Active {
		t.Fatal("active with the switch off = cancel at period end")
	}
	if state.EndTime == nil || !state.EndTime.Equal(expires) {
		t.Fatalf("expiry from expiresDate: %v", state.EndTime)
	}
	if state.Cadence != SubscriptionCadenceYearly {
		t.Fatalf("cadence %q", state.Cadence)
	}

	statuses.Data[0].LastTransactions[0].SignedRenewalInfo = appleTestJws(t, map[string]any{"autoRenewStatus": float64(1)})
	state = appleSubscriptionState(statuses)
	if state.AutoRenew == nil || !*state.AutoRenew || state.CancelAtPeriodEnd {
		t.Fatal("switch on = renews")
	}

	statuses.Data[0].LastTransactions[0].SignedRenewalInfo = ""
	state = appleSubscriptionState(statuses)
	if state.AutoRenew != nil {
		t.Fatal("entitled without renewal info = unknown")
	}

	statuses.Data[0].LastTransactions[0].Status = 2
	state = appleSubscriptionState(statuses)
	if state.AutoRenew == nil || *state.AutoRenew || state.Active {
		t.Fatal("expired = over")
	}

	if appleSubscriptionState(&appleSubscriptionStatusesResponse{}) != nil {
		t.Fatal("no transactions = nothing to say")
	}
}

func TestPlaySubscriptionState(t *testing.T) {
	future := server.NowUtc().Add(20 * 24 * time.Hour).Format(time.RFC3339)
	past := server.NowUtc().Add(-1 * time.Hour).Format(time.RFC3339)
	item := func(expiry string, autoRenew *bool) *PlaySubscriptionPurchaseLineItem {
		li := &PlaySubscriptionPurchaseLineItem{ProductId: "supporter_monthly", ExpiryTime: expiry}
		if autoRenew != nil {
			li.AutoRenewingPlan = &PlayAutoRenewingPlan{AutoRenewEnabled: *autoRenew}
		}
		return li
	}

	state := playSubscriptionState(&PlaySubscription{
		SubscriptionState: "SUBSCRIPTION_STATE_ACTIVE",
		LineItems:         []*PlaySubscriptionPurchaseLineItem{item(future, boolPtr(true))},
	})
	if state.AutoRenew == nil || !*state.AutoRenew || state.CancelAtPeriodEnd || !state.Active || state.EndTime == nil {
		t.Fatal("active + auto renew on")
	}
	if state.Cadence != SubscriptionCadenceMonthly {
		t.Fatalf("cadence %q", state.Cadence)
	}

	state = playSubscriptionState(&PlaySubscription{
		SubscriptionState: "SUBSCRIPTION_STATE_ACTIVE",
		LineItems:         []*PlaySubscriptionPurchaseLineItem{item(future, boolPtr(false))},
	})
	if state.AutoRenew == nil || *state.AutoRenew || !state.CancelAtPeriodEnd {
		t.Fatal("active + auto renew off = cancel at period end")
	}

	state = playSubscriptionState(&PlaySubscription{
		SubscriptionState: "SUBSCRIPTION_STATE_ACTIVE",
		LineItems:         []*PlaySubscriptionPurchaseLineItem{item(future, nil)},
	})
	if state.AutoRenew != nil {
		t.Fatal("no plan block = unknown")
	}

	state = playSubscriptionState(&PlaySubscription{
		SubscriptionState: "SUBSCRIPTION_STATE_CANCELED",
		LineItems:         []*PlaySubscriptionPurchaseLineItem{item(future, boolPtr(false))},
	})
	if state.AutoRenew == nil || *state.AutoRenew || !state.CancelAtPeriodEnd || !state.Active {
		t.Fatal("canceled with time remaining = paid through, no renewal")
	}

	state = playSubscriptionState(&PlaySubscription{
		SubscriptionState: "SUBSCRIPTION_STATE_CANCELED",
		LineItems:         []*PlaySubscriptionPurchaseLineItem{item(past, boolPtr(false))},
	})
	if state.AutoRenew == nil || *state.AutoRenew || state.CancelAtPeriodEnd || state.Active {
		t.Fatal("canceled and expired = over")
	}

	state = playSubscriptionState(&PlaySubscription{
		SubscriptionState: "SUBSCRIPTION_STATE_EXPIRED",
		LineItems:         []*PlaySubscriptionPurchaseLineItem{item(past, boolPtr(true))},
	})
	if state.AutoRenew == nil || *state.AutoRenew || state.Active {
		t.Fatal("expired = over")
	}
}

func TestSubscriptionCancelRefusesStoresItCannotAct(t *testing.T) {
	apple := subscriptionCancelRefusal(model.SubscriptionMarketApple)
	if apple == nil || apple.Error == nil || apple.ManageUrl != appleManageSubscriptionsUrl {
		t.Fatal("apple: cancel in the App Store")
	}
	google := subscriptionCancelRefusal(model.SubscriptionMarketGoogle)
	if google == nil || google.Error == nil || google.ManageUrl != googleManageSubscriptionsUrl {
		t.Fatal("google: cancel in Google Play")
	}
	solana := subscriptionCancelRefusal(model.SubscriptionMarketSolana)
	if solana == nil || solana.Error == nil || solana.ManageUrl != "" {
		t.Fatal("solana: nothing to cancel")
	}
	if subscriptionCancelRefusal("") == nil || subscriptionCancelRefusal("x402") == nil {
		t.Fatal("unknown stores are refused")
	}
	// each refusal names its reason in a stable code beside the message
	for _, c := range []struct {
		store string
		code  string
	}{
		{store: model.SubscriptionMarketApple, code: SubscriptionErrorCodeManagedByAppStore},
		{store: model.SubscriptionMarketGoogle, code: SubscriptionErrorCodeManagedByGooglePlay},
		{store: model.SubscriptionMarketSolana, code: SubscriptionErrorCodeDoesNotRenew},
		{store: "x402", code: SubscriptionErrorCodeNotCancelable},
	} {
		refusal := subscriptionCancelRefusal(c.store)
		if refusal.Error.Code != c.code || refusal.Error.Message == "" {
			t.Errorf("%s: error %+v, want code %s with its message", c.store, refusal.Error, c.code)
		}
	}
	if subscriptionCancelRefusal(model.SubscriptionMarketStripe) != nil {
		t.Fatal("stripe is the store the server acts on")
	}
}

// ----- a fake Stripe for the customer subscription calls -----

type stripeSubscriptionTestEnv struct {
	// customer id -> raw subscription objects the listing serves
	byCustomer map[string][]map[string]any
	// invoice id -> subscription id, for the expanded invoice fetch
	invoiceSubscription map[string]string
	// subscription id -> raw object
	subscriptions map[string]map[string]any
	// the cancel_at_period_end values POSTed, in order
	updates []string

	testServer *httptest.Server
}

func newStripeSubscriptionTestEnv(t testing.TB) *stripeSubscriptionTestEnv {
	env := &stripeSubscriptionTestEnv{
		byCustomer:          map[string][]map[string]any{},
		invoiceSubscription: map[string]string{},
		subscriptions:       map[string]map[string]any{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk_test_details" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		data := []map[string]any{}
		for _, id := range env.customerSubscriptionIds(r.URL.Query().Get("customer")) {
			data = append(data, env.subscriptions[id])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "has_more": false})
	})
	mux.HandleFunc("GET /v1/invoices/{id}", func(w http.ResponseWriter, r *http.Request) {
		subscriptionId, ok := env.invoiceSubscription[r.PathValue("id")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "subscription": env.subscriptions[subscriptionId]})
	})
	mux.HandleFunc("POST /v1/subscriptions/{id}", func(w http.ResponseWriter, r *http.Request) {
		sub, ok := env.subscriptions[r.PathValue("id")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		value := r.PostForm.Get("cancel_at_period_end")
		env.updates = append(env.updates, value)
		sub["cancel_at_period_end"] = value == "true"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sub)
	})
	env.testServer = httptest.NewServer(mux)

	prevBaseUrl := stripeApiBaseUrl
	prevTokenFunc := stripeApiTokenFunc
	stripeApiBaseUrl = env.testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_details" }
	t.Cleanup(func() {
		stripeApiBaseUrl = prevBaseUrl
		stripeApiTokenFunc = prevTokenFunc
		env.testServer.Close()
	})
	return env
}

func (self *stripeSubscriptionTestEnv) customerSubscriptionIds(customerId string) []string {
	ids := []string{}
	for _, sub := range self.byCustomer[customerId] {
		ids = append(ids, sub["id"].(string))
	}
	return ids
}

func (self *stripeSubscriptionTestEnv) addSubscription(customerId string, id string, status string, cancelAtPeriodEnd bool, periodEnd time.Time, interval string) map[string]any {
	sub := map[string]any{
		"id":                   id,
		"customer":             customerId,
		"status":               status,
		"cancel_at_period_end": cancelAtPeriodEnd,
		"created":              server.NowUtc().Unix(),
		"items": map[string]any{
			"data": []map[string]any{{
				"current_period_end": periodEnd.Unix(),
				"price":              map[string]any{"id": "price_" + interval, "recurring": map[string]any{"interval": interval}},
			}},
		},
	}
	self.subscriptions[id] = sub
	self.byCustomer[customerId] = append(self.byCustomer[customerId], sub)
	return sub
}

func TestStripeFindSubscriptionPrefersTheBillingOne(t *testing.T) {
	env := newStripeSubscriptionTestEnv(t)
	now := server.NowUtc()
	env.addSubscription("cus_1", "sub_old", "canceled", false, now.Add(-100*24*time.Hour), "month")
	env.addSubscription("cus_1", "sub_live", "active", false, now.Add(300*24*time.Hour), "year")
	env.invoiceSubscription["in_1"] = "sub_live"

	sub, err := stripeFindSubscription(context.Background(), "cus_1", "")
	if err != nil || sub.Id != "sub_live" {
		t.Fatalf("expected the billing subscription first, got %v %v", sub, err)
	}
	// no customer row: through the renewal's invoice
	sub, err = stripeFindSubscription(context.Background(), "", "in_1")
	if err != nil || sub.Id != "sub_live" {
		t.Fatalf("expected the invoice's subscription, got %v %v", sub, err)
	}
	// a customer with nothing listed falls back to the invoice too
	sub, err = stripeFindSubscription(context.Background(), "cus_none", "in_1")
	if err != nil || sub.Id != "sub_live" {
		t.Fatalf("expected the invoice fallback, got %v %v", sub, err)
	}
	if _, err := stripeFindSubscription(context.Background(), "cus_none", ""); err == nil {
		t.Fatal("nothing to look up is an error (auto_renew unknown), not a fake state")
	}

	state, err := stripeLookupSubscriptionState(context.Background(), "cus_1", "")
	if err != nil || state.AutoRenew == nil || !*state.AutoRenew || state.Cadence != SubscriptionCadenceYearly {
		t.Fatalf("lookup state: %+v %v", state, err)
	}

	// the write: cancel, then resume
	updated, err := stripeSetCancelAtPeriodEnd(context.Background(), "sub_live", true)
	if err != nil || !updated.CancelAtPeriodEnd {
		t.Fatalf("cancel: %+v %v", updated, err)
	}
	updated, err = stripeSetCancelAtPeriodEnd(context.Background(), "sub_live", false)
	if err != nil || updated.CancelAtPeriodEnd {
		t.Fatalf("resume: %+v %v", updated, err)
	}
	if len(env.updates) != 2 || env.updates[0] != "true" || env.updates[1] != "false" {
		t.Fatalf("expected the form updates [true false], got %v", env.updates)
	}
}
