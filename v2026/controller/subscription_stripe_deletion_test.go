package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type stripeDeletionFixture struct {
	testing.TB

	mutex sync.Mutex

	networkId       server.Id
	invoices        map[string]*stripeCustomerSubscription
	listPages       map[string]*stripeCustomerSubscriptionList
	search          map[string]*stripeCustomerSubscriptionList
	deleted         map[string]int
	requests        []string
	networkPresent  func() bool
	presentOnDelete []bool

	searchStatus int
	server       *httptest.Server
}

func newStripeDeletionFixture(t testing.TB, networkId server.Id) *stripeDeletionFixture {
	t.Helper()
	fixture := &stripeDeletionFixture{
		TB:           t,
		networkId:    networkId,
		invoices:     map[string]*stripeCustomerSubscription{},
		listPages:    map[string]*stripeCustomerSubscriptionList{},
		search:       map[string]*stripeCustomerSubscriptionList{},
		deleted:      map[string]int{},
		searchStatus: http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/invoices/{invoiceId}", fixture.serveInvoice)
	mux.HandleFunc("GET /v1/subscriptions", fixture.serveList)
	mux.HandleFunc("GET /v1/subscriptions/search", fixture.serveSearch)
	mux.HandleFunc("DELETE /v1/subscriptions/{subscriptionId}", fixture.serveDelete)
	fixture.server = httptest.NewServer(mux)

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	stripeApiBaseUrl = fixture.server.URL
	stripeApiTokenFunc = func() string { return "synthetic-provider-token" }
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		fixture.server.Close()
	})
	return fixture
}

func (f *stripeDeletionFixture) record(request *http.Request) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.requests = append(f.requests, request.Method+" "+request.URL.RequestURI())
}

func (f *stripeDeletionFixture) authorize(response http.ResponseWriter, request *http.Request) bool {
	if request.Header.Get("Authorization") != "Bearer synthetic-provider-token" {
		http.Error(response, "synthetic unauthorized", http.StatusUnauthorized)
		return false
	}
	f.record(request)
	return true
}

func (f *stripeDeletionFixture) encode(response http.ResponseWriter, value any) {
	f.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		f.Errorf("encode synthetic Stripe response: %v", err)
	}
}

func (f *stripeDeletionFixture) serveInvoice(response http.ResponseWriter, request *http.Request) {
	if !f.authorize(response, request) {
		return
	}
	f.mutex.Lock()
	sub := f.invoices[request.PathValue("invoiceId")]
	f.mutex.Unlock()
	if sub == nil {
		http.Error(response, "synthetic invoice missing", http.StatusNotFound)
		return
	}
	f.encode(response, &stripeInvoiceWithSubscription{Subscription: sub})
}

func (f *stripeDeletionFixture) serveList(response http.ResponseWriter, request *http.Request) {
	if !f.authorize(response, request) {
		return
	}
	query := request.URL.Query()
	if query.Get("customer") != "cus_synthetic_delete" || query.Get("status") != "all" || query.Get("limit") != "100" {
		http.Error(response, "synthetic list query mismatch", http.StatusBadRequest)
		return
	}
	f.mutex.Lock()
	page, ok := f.listPages[query.Get("starting_after")]
	f.mutex.Unlock()
	if !ok {
		http.Error(response, "synthetic list page missing", http.StatusBadRequest)
		return
	}
	f.encode(response, page)
}

func (f *stripeDeletionFixture) serveSearch(response http.ResponseWriter, request *http.Request) {
	if !f.authorize(response, request) {
		return
	}
	if f.searchStatus != http.StatusOK {
		http.Error(response, "synthetic search failure", f.searchStatus)
		return
	}
	query := request.URL.Query()
	wantQuery := fmt.Sprintf("metadata['%s']:'%s'", stripeMetadataNetworkId, f.networkId)
	if query.Get("query") != wantQuery || query.Get("limit") != "100" {
		http.Error(response, "synthetic search query mismatch", http.StatusBadRequest)
		return
	}
	f.mutex.Lock()
	page, ok := f.search[query.Get("page")]
	f.mutex.Unlock()
	if !ok {
		http.Error(response, "synthetic search page missing", http.StatusBadRequest)
		return
	}
	f.encode(response, page)
}

func (f *stripeDeletionFixture) serveDelete(response http.ResponseWriter, request *http.Request) {
	if !f.authorize(response, request) {
		return
	}
	subscriptionId, err := url.PathUnescape(request.PathValue("subscriptionId"))
	if err != nil {
		http.Error(response, "synthetic subscription id malformed", http.StatusBadRequest)
		return
	}
	f.mutex.Lock()
	f.deleted[subscriptionId] += 1
	if f.networkPresent != nil {
		f.presentOnDelete = append(f.presentOnDelete, f.networkPresent())
	}
	f.mutex.Unlock()
	f.encode(response, &stripeCustomerSubscription{Id: subscriptionId, Status: "canceled"})
}

func stripeDeletionSubscriptionFixture(id string, status string, networkId server.Id, periodEnd int64) *stripeCustomerSubscription {
	return &stripeCustomerSubscription{
		Id:               id,
		Status:           status,
		CurrentPeriodEnd: periodEnd,
		Metadata: map[string]string{
			stripeMetadataNetworkId: networkId.String(),
		},
	}
}

func TestStripeDeletionDiscoversEveryPageAndCancelsMetadataOnlySubscriptions(t *testing.T) {
	networkId := server.NewId()
	fixture := newStripeDeletionFixture(t, networkId)
	fromInvoice := stripeDeletionSubscriptionFixture("sub_synthetic_invoice", "active", networkId, 10)
	fromCustomer := stripeDeletionSubscriptionFixture("sub_synthetic_customer", "past_due", networkId, 20)
	metadataOnly := stripeDeletionSubscriptionFixture("sub_synthetic_metadata", "trialing", networkId, 30)
	alreadyTerminal := stripeDeletionSubscriptionFixture("sub_synthetic_terminal", "canceled", networkId, 40)
	fixture.invoices["in_synthetic_delete"] = fromInvoice
	fixture.listPages[""] = &stripeCustomerSubscriptionList{
		Data:    []*stripeCustomerSubscription{fromInvoice, fromCustomer},
		HasMore: true,
	}
	fixture.listPages[fromCustomer.Id] = &stripeCustomerSubscriptionList{
		Data: []*stripeCustomerSubscription{fromCustomer, alreadyTerminal},
	}
	fixture.search[""] = &stripeCustomerSubscriptionList{
		Data:     []*stripeCustomerSubscription{fromCustomer, metadataOnly},
		HasMore:  true,
		NextPage: "synthetic-page-two",
	}
	fixture.search["synthetic-page-two"] = &stripeCustomerSubscriptionList{
		Data: []*stripeCustomerSubscription{metadataOnly, fromInvoice},
	}

	deletions, err := stripeDiscoverDeletionSubscriptions(
		context.Background(),
		networkId,
		"cus_synthetic_delete",
		[]string{"in_synthetic_delete"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletions) != 4 {
		t.Fatalf("discovered %d subscriptions, want 4", len(deletions))
	}
	closed := []string{}
	if err := stripeCancelDeletionSubscriptions(
		context.Background(),
		deletions,
		func(invoiceId string) error {
			closed = append(closed, invoiceId)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	sort.Strings(closed)
	if !reflect.DeepEqual(closed, []string{"in_synthetic_delete"}) {
		t.Fatalf("closed local renewals = %v", closed)
	}
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	wantDeleted := map[string]int{
		fromInvoice.Id:  1,
		fromCustomer.Id: 1,
		metadataOnly.Id: 1,
	}
	if !reflect.DeepEqual(fixture.deleted, wantDeleted) {
		t.Fatalf("provider cancellations = %v, want %v", fixture.deleted, wantDeleted)
	}
}

func TestStripeDeletionDiscoveryFailsBeforeAnyCancellation(t *testing.T) {
	networkId := server.NewId()
	fixture := newStripeDeletionFixture(t, networkId)
	listed := stripeDeletionSubscriptionFixture("sub_synthetic_listed", "active", networkId, 10)
	fixture.listPages[""] = &stripeCustomerSubscriptionList{Data: []*stripeCustomerSubscription{listed}}
	fixture.searchStatus = http.StatusServiceUnavailable

	deletions, err := stripeDiscoverDeletionSubscriptions(
		context.Background(),
		networkId,
		"cus_synthetic_delete",
		nil,
	)
	if err == nil {
		t.Fatal("provider search failure was accepted as complete discovery")
	}
	if deletions != nil {
		t.Fatalf("failed discovery returned %d cancellable subscriptions", len(deletions))
	}
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if len(fixture.deleted) != 0 {
		t.Fatalf("failed discovery canceled provider subscriptions: %v", fixture.deleted)
	}
}

func TestStripeSubscriptionPaginationRejectsCycles(t *testing.T) {
	networkId := server.NewId()
	fixture := newStripeDeletionFixture(t, networkId)
	cycled := stripeDeletionSubscriptionFixture("sub_synthetic_cycle", "active", networkId, 10)
	fixture.listPages[""] = &stripeCustomerSubscriptionList{
		Data:    []*stripeCustomerSubscription{cycled},
		HasMore: true,
	}
	fixture.listPages[cycled.Id] = &stripeCustomerSubscriptionList{
		Data:    []*stripeCustomerSubscription{cycled},
		HasMore: true,
	}

	if _, err := stripeListCustomerSubscriptions(context.Background(), "cus_synthetic_delete"); err == nil {
		t.Fatal("cyclic customer pagination was accepted")
	}
}

// Network deletion must search exact provider metadata even when no active
// local renewal and no Stripe-customer row survived. The provider object is
// canceled while the network still exists, then local deletion may proceed.
func TestNetworkRemoveCancelsStripeSubscriptionKnownOnlyByMetadata(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-stripe-metadata-remove", userId)
		userSession := networkRemoveTestSession(ctx, networkId, userId)
		fixture := newStripeDeletionFixture(t, networkId)
		metadataOnly := stripeDeletionSubscriptionFixture(
			"sub_synthetic_metadata_only",
			"active",
			networkId,
			10,
		)
		fixture.search[""] = &stripeCustomerSubscriptionList{
			Data: []*stripeCustomerSubscription{metadataOnly},
		}
		fixture.networkPresent = func() bool {
			return model.GetNetwork(userSession) != nil
		}

		result, err := NetworkRemove(userSession)
		if err != nil || result == nil || result.Error != nil {
			t.Fatalf("remove metadata-only Stripe network = %#v, %v", result, err)
		}
		if model.GetNetwork(userSession) != nil {
			t.Fatal("network remained after provider-confirmed cancellation")
		}
		fixture.mutex.Lock()
		deletes := fixture.deleted[metadataOnly.Id]
		presentOnDelete := append([]bool{}, fixture.presentOnDelete...)
		fixture.mutex.Unlock()
		if deletes != 1 {
			t.Fatalf("metadata-only provider cancellation count = %d, want 1", deletes)
		}
		if !reflect.DeepEqual(presentOnDelete, []bool{true}) {
			t.Fatalf("network presence during provider cancellation = %v, want [true]", presentOnDelete)
		}
	})
}

// Provider discovery is part of deletion correctness, not an optional audit.
// A failed exact-metadata search retains the local account and emits no
// cancellation so an incomplete provider view can never be treated as empty.
func TestNetworkRemoveRetainsNetworkWhenStripeDiscoveryFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-stripe-search-failure", userId)
		userSession := networkRemoveTestSession(ctx, networkId, userId)
		fixture := newStripeDeletionFixture(t, networkId)
		fixture.searchStatus = http.StatusServiceUnavailable

		result, err := NetworkRemove(userSession)
		if err != nil {
			t.Fatalf("remove returned transport error: %v", err)
		}
		if result == nil || result.Error == nil {
			t.Fatal("network deletion succeeded with incomplete Stripe discovery")
		}
		if model.GetNetwork(userSession) == nil {
			t.Fatal("Stripe discovery failure deleted the network")
		}
		fixture.mutex.Lock()
		deletes := len(fixture.deleted)
		fixture.mutex.Unlock()
		if deletes != 0 {
			t.Fatalf("failed discovery issued %d provider cancellation(s)", deletes)
		}
	})
}
