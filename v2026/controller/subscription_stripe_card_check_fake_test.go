package controller

// A fake Stripe api for the trial card check and the abandoned-sheet sweep:
// the raw-HTTP reads (stripeApiBaseUrl) and the stripe-go calls (its api
// backend) alike, keeping what each write did. Shared by the hermetic and the
// database tests.

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/server/v2026"
)

type stripeCardCheckFake struct {
	stateLock sync.Mutex

	subscriptions  map[string]map[string]any
	invoices       map[string]map[string]any
	paymentMethods map[string]map[string]any
	customers      map[string]map[string]any
	sessions       map[string]map[string]any
	// the paid invoices the reconcile lists
	listInvoices []map[string]any

	// "METHOD /path" -> the status the fake answers it with instead
	failRoutes map[string]int

	// every request, as "METHOD /path"
	requests []string
	// the subscription updates, as "<id> <form>"
	updates []string
	// the payment methods detached
	detached []string
	// the subscriptions canceled
	canceled []string
	// the search queries
	searchQueries []string
	// runs, with stateLock held, after each search is answered: what changes
	// at Stripe between the search and the reads after it
	afterSearch func()
	// subscription updates by idempotency key: a repeated key is answered the
	// first answer, replayed, and changes nothing, as Stripe does
	idempotent map[string]map[string]any
}

// Serves the fake through both seams, restored when the test ends.
func newStripeCardCheckFake(t testing.TB) *stripeCardCheckFake {
	fake := &stripeCardCheckFake{
		idempotent:     map[string]map[string]any{},
		subscriptions:  map[string]map[string]any{},
		invoices:       map[string]map[string]any{},
		paymentMethods: map[string]map[string]any{},
		customers:      map[string]map[string]any{},
		sessions:       map[string]map[string]any{},
		failRoutes:     map[string]int{},
	}
	testServer := httptest.NewServer(http.HandlerFunc(fake.serve))

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousToken := stripeApiToken
	previousBackend := stripe.GetBackend(stripe.APIBackend)
	previousKey := stripe.Key
	stripeApiBaseUrl = testServer.URL
	stripeApiTokenFunc = func() string { return "sk_test_card_check" }
	stripeApiToken = func() string { return "sk_test_card_check" }
	stripe.Key = "sk_test_card_check"
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(testServer.URL),
		MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		stripeApiToken = previousToken
		stripe.Key = previousKey
		stripe.SetBackend(stripe.APIBackend, previousBackend)
		testServer.Close()
	})
	return fake
}

// ----- objects -----

// A card whose CVC check answered cvcCheck ("" for no check at all).
func (self *stripeCardCheckFake) addCard(paymentMethodId string, customerId string, cvcCheck string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	card := map[string]any{"brand": "visa", "last4": "4242"}
	if cvcCheck != "" {
		card["checks"] = map[string]any{
			"cvc_check":                 cvcCheck,
			"address_line1_check":       nil,
			"address_postal_code_check": nil,
		}
	}
	self.paymentMethods[paymentMethodId] = map[string]any{
		"id":       paymentMethodId,
		"object":   "payment_method",
		"type":     "card",
		"customer": customerId,
		"card":     card,
	}
}

// A customer, with its invoice default payment method ("" for none).
func (self *stripeCardCheckFake) addCustomer(customerId string, defaultPaymentMethod string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var paymentMethod any
	if defaultPaymentMethod != "" {
		paymentMethod = defaultPaymentMethod
	}
	self.customers[customerId] = map[string]any{
		"id":               customerId,
		"object":           "customer",
		"email":            "buyer@example.com",
		"invoice_settings": map[string]any{"default_payment_method": paymentMethod},
		"default_source":   nil,
	}
}

// A subscription of the network: status, metadata beside network_id, its
// default payment method ("" for none) and latest invoice, created at created
// with its trial ending at trialEnd (zero for none).
func (self *stripeCardCheckFake) addSubscription(
	subscriptionId string,
	customerId string,
	networkId server.Id,
	status string,
	metadata map[string]string,
	defaultPaymentMethod string,
	invoiceId string,
	created time.Time,
	trialEnd time.Time,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	allMetadata := map[string]any{"network_id": networkId.String()}
	for key, value := range metadata {
		allMetadata[key] = value
	}
	var paymentMethod any
	if defaultPaymentMethod != "" {
		paymentMethod = defaultPaymentMethod
	}
	var trialEndUnix any
	if !trialEnd.IsZero() {
		trialEndUnix = trialEnd.Unix()
	}
	var latestInvoice any
	if invoiceId != "" {
		latestInvoice = invoiceId
	}
	self.subscriptions[subscriptionId] = map[string]any{
		"id":                     subscriptionId,
		"object":                 "subscription",
		"status":                 status,
		"customer":               customerId,
		"metadata":               allMetadata,
		"default_payment_method": paymentMethod,
		"default_source":         nil,
		"latest_invoice":         latestInvoice,
		"created":                created.Unix(),
		"trial_end":              trialEndUnix,
		"items":                  map[string]any{"object": "list", "data": []any{}},
	}
}

// A paid invoice of the subscription for total (0: a trial's), billing
// [periodStart, periodEnd).
func (self *stripeCardCheckFake) addInvoice(invoiceId string, subscriptionId string, total int, periodStart time.Time, periodEnd time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	customerId, _ := self.subscriptions[subscriptionId]["customer"].(string)
	self.invoices[invoiceId] = map[string]any{
		"id":             invoiceId,
		"object":         "invoice",
		"status":         "paid",
		"total":          total,
		"amount_paid":    total,
		"currency":       "usd",
		"billing_reason": "subscription_create",
		"customer":       customerId,
		"subscription":   subscriptionId,
		"charge":         nil,
		"lines": map[string]any{"data": []map[string]any{{
			"type":         "subscription",
			"subscription": subscriptionId,
			"period":       map[string]any{"start": periodStart.Unix(), "end": periodEnd.Unix()},
		}}},
	}
}

// A completed Pro checkout of the network that started the subscription.
func (self *stripeCardCheckFake) addCheckout(sessionId string, networkId server.Id, subscriptionId string, invoiceId string, customerId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sessions[sessionId] = map[string]any{
		"id":                  sessionId,
		"object":              "checkout.session",
		"mode":                "subscription",
		"status":              "complete",
		"payment_status":      "no_payment_required",
		"client_reference_id": networkId.String(),
		"customer":            customerId,
		"subscription":        subscriptionId,
		"invoice":             invoiceId,
		"created":             server.NowUtc().Add(-time.Minute).Unix(),
	}
}

// The subscription's metadata now.
func (self *stripeCardCheckFake) metadata(subscriptionId string) map[string]string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	out := map[string]string{}
	stored, _ := self.subscriptions[subscriptionId]["metadata"].(map[string]any)
	for key, value := range stored {
		out[key], _ = value.(string)
	}
	return out
}

// The subscription's status now.
func (self *stripeCardCheckFake) status(subscriptionId string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	status, _ := self.subscriptions[subscriptionId]["status"].(string)
	return status
}

// The subscription's default payment method now, "" for none.
func (self *stripeCardCheckFake) defaultPaymentMethod(subscriptionId string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	paymentMethod, _ := self.subscriptions[subscriptionId]["default_payment_method"].(string)
	return paymentMethod
}

// Removes a metadata key of a stored subscription.
func (self *stripeCardCheckFake) deleteMetadata(subscriptionId string, key string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	metadata, _ := self.subscriptions[subscriptionId]["metadata"].(map[string]any)
	delete(metadata, key)
}

// Sets a field of a stored subscription.
func (self *stripeCardCheckFake) setSubscriptionField(subscriptionId string, key string, value any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.subscriptions[subscriptionId][key] = value
}

// Answers the route with status instead (0 to stop).
func (self *stripeCardCheckFake) failRoute(route string, status int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if status == 0 {
		delete(self.failRoutes, route)
	} else {
		self.failRoutes[route] = status
	}
}

// What the writes did so far.
func (self *stripeCardCheckFake) writes() (updates []string, detached []string, canceled []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.updates), slices.Clone(self.detached), slices.Clone(self.canceled)
}

// How many requests matched "METHOD /path" with the path's prefix.
func (self *stripeCardCheckFake) requestCount(methodAndPrefix string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, request := range self.requests {
		if strings.HasPrefix(request, methodAndPrefix) {
			count += 1
		}
	}
	return count
}

// ----- the api -----

var (
	stripeCardCheckFakeSubscriptionPath = regexp.MustCompile(`^/v1/subscriptions/([A-Za-z0-9_]+)$`)
	stripeCardCheckFakeInvoicePath      = regexp.MustCompile(`^/v1/invoices/([A-Za-z0-9_]+)$`)
	stripeCardCheckFakeSessionPath      = regexp.MustCompile(`^/v1/checkout/sessions/([A-Za-z0-9_]+)$`)
	stripeCardCheckFakeMethodPath       = regexp.MustCompile(`^/v1/payment_methods/([A-Za-z0-9_]+)$`)
	stripeCardCheckFakeDetachPath       = regexp.MustCompile(`^/v1/payment_methods/([A-Za-z0-9_]+)/detach$`)
	stripeCardCheckFakeCustomerPath     = regexp.MustCompile(`^/v1/customers/([A-Za-z0-9_]+)$`)
	stripeCardCheckFakeSearchStatus     = regexp.MustCompile(`status:'([a-z_]+)'`)
)

// The fields the request asks to expand, as stripe-go (expand[0]=...) and the
// raw reads (expand[]=...) send them.
func stripeCardCheckFakeExpand(r *http.Request) map[string]bool {
	expand := map[string]bool{}
	for key, values := range r.URL.Query() {
		if strings.HasPrefix(key, "expand[") {
			for _, value := range values {
				expand[value] = true
			}
		}
	}
	if r.Form != nil {
		for key, values := range r.Form {
			if strings.HasPrefix(key, "expand[") {
				for _, value := range values {
					expand[value] = true
				}
			}
		}
	}
	return expand
}

func (self *stripeCardCheckFake) serve(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	route := r.Method + " " + r.URL.Path
	self.stateLock.Lock()
	self.requests = append(self.requests, route)
	failStatus := 0
	for failRoute, status := range self.failRoutes {
		if failRoute == route || (strings.HasSuffix(failRoute, "*") && strings.HasPrefix(route, strings.TrimSuffix(failRoute, "*"))) {
			failStatus = status
		}
	}
	self.stateLock.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if failStatus != 0 {
		w.WriteHeader(failStatus)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "api_error", "message": "synthetic failure"}})
		return
	}
	expand := stripeCardCheckFakeExpand(r)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "resource_missing", "message": "No such object"}})
	}
	answer := func(object any) {
		json.NewEncoder(w).Encode(object)
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/search":
		query := r.Form.Get("query")
		self.searchQueries = append(self.searchQueries, query)
		status := ""
		if match := stripeCardCheckFakeSearchStatus.FindStringSubmatch(query); match != nil {
			status = match[1]
		}
		data := []any{}
		for _, id := range slices.Sorted(maps.Keys(self.subscriptions)) {
			sub := self.subscriptions[id]
			metadata, _ := sub["metadata"].(map[string]any)
			// the age is the caller's to judge: what the search returns may lag
			if metadata["payment_sheet"] == "pending" && sub["status"] == status {
				data = append(data, self.subscriptionObject(sub, nil))
			}
		}
		answer(map[string]any{"object": "search_result", "data": data, "has_more": false, "next_page": nil, "url": "/v1/subscriptions/search"})
		if self.afterSearch != nil {
			self.afterSearch()
		}

	case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions":
		customerId := r.Form.Get("customer")
		data := []any{}
		for _, id := range slices.Sorted(maps.Keys(self.subscriptions)) {
			sub := self.subscriptions[id]
			if customerId == "" || sub["customer"] == customerId {
				data = append(data, self.subscriptionObject(sub, map[string]bool{"default_payment_method": expand["data.default_payment_method"]}))
			}
		}
		answer(map[string]any{"object": "list", "data": data, "has_more": false, "url": "/v1/subscriptions"})

	case stripeCardCheckFakeSubscriptionPath.MatchString(r.URL.Path):
		id := stripeCardCheckFakeSubscriptionPath.FindStringSubmatch(r.URL.Path)[1]
		sub, ok := self.subscriptions[id]
		if !ok {
			notFound()
			return
		}
		switch r.Method {
		case http.MethodPost:
			idempotencyKey := r.Header.Get("Idempotency-Key")
			if replay, ok := self.idempotent[idempotencyKey]; ok {
				w.Header().Set("Idempotent-Replayed", "true")
				answer(replay)
				return
			}
			defer func() {
				self.idempotent[idempotencyKey] = self.subscriptionObject(sub, expand)
			}()
			form := []string{}
			for key, values := range r.PostForm {
				form = append(form, fmt.Sprintf("%s=%s", key, strings.Join(values, ",")))
				if strings.HasPrefix(key, "metadata[") {
					metadata, _ := sub["metadata"].(map[string]any)
					name := strings.TrimSuffix(strings.TrimPrefix(key, "metadata["), "]")
					if values[0] == "" {
						delete(metadata, name)
					} else {
						metadata[name] = values[0]
					}
				}
				if key == "default_payment_method" {
					if values[0] == "" {
						sub["default_payment_method"] = nil
					} else {
						sub["default_payment_method"] = values[0]
					}
				}
			}
			slices.Sort(form)
			self.updates = append(self.updates, id+" "+strings.Join(form, "&"))
		case http.MethodDelete:
			sub["status"] = "canceled"
			sub["canceled_at"] = server.NowUtc().Unix()
			self.canceled = append(self.canceled, id)
		}
		answer(self.subscriptionObject(sub, expand))

	case r.Method == http.MethodGet && r.URL.Path == "/v1/invoices":
		answer(map[string]any{"object": "list", "data": self.listInvoices, "has_more": false})

	case r.Method == http.MethodGet && stripeCardCheckFakeInvoicePath.MatchString(r.URL.Path):
		id := stripeCardCheckFakeInvoicePath.FindStringSubmatch(r.URL.Path)[1]
		invoice, ok := self.invoices[id]
		if !ok {
			notFound()
			return
		}
		object := maps.Clone(invoice)
		if expand["subscription"] {
			subscriptionId, _ := invoice["subscription"].(string)
			if sub, ok := self.subscriptions[subscriptionId]; ok {
				object["subscription"] = map[string]any{"id": subscriptionId, "status": sub["status"], "metadata": sub["metadata"]}
			}
		}
		if expand["customer"] {
			customerId, _ := invoice["customer"].(string)
			object["customer"] = map[string]any{"id": customerId, "email": "buyer@example.com"}
		}
		answer(object)

	case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
		subscriptionId := r.Form.Get("subscription")
		data := []any{}
		for _, id := range slices.Sorted(maps.Keys(self.sessions)) {
			if self.sessions[id]["subscription"] == subscriptionId {
				data = append(data, self.sessions[id])
			}
		}
		answer(map[string]any{"data": data, "has_more": false})

	case r.Method == http.MethodGet && stripeCardCheckFakeSessionPath.MatchString(r.URL.Path):
		id := stripeCardCheckFakeSessionPath.FindStringSubmatch(r.URL.Path)[1]
		checkout, ok := self.sessions[id]
		if !ok {
			notFound()
			return
		}
		answer(checkout)

	case r.Method == http.MethodGet && stripeCardCheckFakeMethodPath.MatchString(r.URL.Path):
		id := stripeCardCheckFakeMethodPath.FindStringSubmatch(r.URL.Path)[1]
		paymentMethod, ok := self.paymentMethods[id]
		if !ok {
			notFound()
			return
		}
		answer(paymentMethod)

	case r.Method == http.MethodGet && stripeCardCheckFakeCustomerPath.MatchString(r.URL.Path):
		id := stripeCardCheckFakeCustomerPath.FindStringSubmatch(r.URL.Path)[1]
		stored, ok := self.customers[id]
		if !ok {
			notFound()
			return
		}
		object := maps.Clone(stored)
		if expand["invoice_settings.default_payment_method"] {
			settings, _ := stored["invoice_settings"].(map[string]any)
			if paymentMethodId, ok := settings["default_payment_method"].(string); ok {
				if paymentMethod, ok := self.paymentMethods[paymentMethodId]; ok {
					object["invoice_settings"] = map[string]any{"default_payment_method": paymentMethod}
				}
			}
		}
		answer(object)

	case r.Method == http.MethodPost && stripeCardCheckFakeDetachPath.MatchString(r.URL.Path):
		id := stripeCardCheckFakeDetachPath.FindStringSubmatch(r.URL.Path)[1]
		paymentMethod, ok := self.paymentMethods[id]
		if !ok {
			notFound()
			return
		}
		paymentMethod["customer"] = nil
		self.detached = append(self.detached, id)
		answer(paymentMethod)

	default:
		notFound()
	}
}

// The subscription as Stripe answers it, with the asked fields expanded.
// stateLock must be held.
func (self *stripeCardCheckFake) subscriptionObject(sub map[string]any, expand map[string]bool) map[string]any {
	object := maps.Clone(sub)
	if expand["default_payment_method"] {
		if paymentMethodId, ok := sub["default_payment_method"].(string); ok {
			if paymentMethod, ok := self.paymentMethods[paymentMethodId]; ok {
				object["default_payment_method"] = paymentMethod
			}
		}
	}
	if expand["customer"] {
		if customerId, ok := sub["customer"].(string); ok {
			if customer, ok := self.customers[customerId]; ok {
				object["customer"] = customer
			}
		}
	}
	if expand["latest_invoice"] {
		if invoiceId, ok := sub["latest_invoice"].(string); ok {
			if invoice, ok := self.invoices[invoiceId]; ok {
				object["latest_invoice"] = invoice
			}
		}
	}
	return object
}
