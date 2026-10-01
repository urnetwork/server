package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// networkRemoveStripeFake serves synthetic Stripe invoice and cancellation
// responses while recording which provider operations ran.
type networkRemoveStripeFake struct {
	lock sync.Mutex

	invoices          map[string]string
	subscriptionState map[string]string
	deleteStatus      map[string][]int
	deleteResponseId  map[string]string
	deleteResponse    map[string]string
	getCount          map[string]int
	deleteCount       map[string]int
	networkPresent    func() bool
	presentOnDelete   []bool

	server *httptest.Server
}

// newNetworkRemoveStripeFake installs one per-test synthetic Stripe endpoint.
func newNetworkRemoveStripeFake(t testing.TB) *networkRemoveStripeFake {
	t.Helper()
	fake := &networkRemoveStripeFake{
		invoices:          map[string]string{},
		subscriptionState: map[string]string{},
		deleteStatus:      map[string][]int{},
		deleteResponseId:  map[string]string{},
		deleteResponse:    map[string]string{},
		getCount:          map[string]int{},
		deleteCount:       map[string]int{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/invoices/{invoiceId}", func(response http.ResponseWriter, request *http.Request) {
		invoiceId := request.PathValue("invoiceId")
		fake.lock.Lock()
		defer fake.lock.Unlock()
		fake.getCount[invoiceId]++
		subscriptionId, ok := fake.invoices[invoiceId]
		if !ok {
			http.Error(response, "synthetic invoice not found", http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(response).Encode(map[string]any{
			"subscription": map[string]any{
				"id":     subscriptionId,
				"status": fake.subscriptionState[subscriptionId],
			},
		}); err != nil {
			t.Errorf("encode invoice: %v", err)
		}
	})
	mux.HandleFunc("GET /v1/subscriptions/search", func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(response).Encode(map[string]any{
			"data":     []any{},
			"has_more": false,
		}); err != nil {
			t.Errorf("encode empty subscription search: %v", err)
		}
	})
	mux.HandleFunc("DELETE /v1/subscriptions/{subscriptionId}", func(response http.ResponseWriter, request *http.Request) {
		subscriptionId := request.PathValue("subscriptionId")
		fake.lock.Lock()
		defer fake.lock.Unlock()
		fake.deleteCount[subscriptionId]++
		if fake.networkPresent != nil {
			fake.presentOnDelete = append(fake.presentOnDelete, fake.networkPresent())
		}
		status := http.StatusOK
		if statuses := fake.deleteStatus[subscriptionId]; 0 < len(statuses) {
			status = statuses[0]
			fake.deleteStatus[subscriptionId] = statuses[1:]
		}
		if status < 200 || 300 <= status {
			http.Error(response, "synthetic cancellation failure", status)
			return
		}
		responseId := subscriptionId
		if configured := fake.deleteResponseId[subscriptionId]; configured != "" {
			responseId = configured
		}
		responseStatus := "canceled"
		if configured := fake.deleteResponse[subscriptionId]; configured != "" {
			responseStatus = configured
		}
		fake.subscriptionState[subscriptionId] = responseStatus
		response.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(response).Encode(map[string]any{
			"id":     responseId,
			"status": responseStatus,
		}); err != nil {
			t.Errorf("encode subscription: %v", err)
		}
	})
	fake.server = httptest.NewServer(mux)

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	stripeApiBaseUrl = fake.server.URL
	stripeApiTokenFunc = func() string { return "synthetic-test-token" }
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		fake.server.Close()
	})
	return fake
}

// add configures one synthetic invoice and its provider subscription.
func (self *networkRemoveStripeFake) add(invoiceId string, subscriptionId string, status string, deleteStatus ...int) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.invoices[invoiceId] = subscriptionId
	self.subscriptionState[subscriptionId] = status
	self.deleteStatus[subscriptionId] = append([]int{}, deleteStatus...)
}

// setDeleteResponse overrides the synthetic object returned by a successful
// cancellation request.
func (self *networkRemoveStripeFake) setDeleteResponse(subscriptionId string, responseId string, status string) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.deleteResponseId[subscriptionId] = responseId
	self.deleteResponse[subscriptionId] = status
}

// counts returns GET-invoice and DELETE-subscription call counts.
func (self *networkRemoveStripeFake) counts(invoiceId string, subscriptionId string) (int, int) {
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.getCount[invoiceId], self.deleteCount[subscriptionId]
}

// addNetworkRemoveStripeRenewal seeds one synthetic local Stripe renewal.
func addNetworkRemoveStripeRenewal(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	transactionId string,
	startTime time.Time,
	endTime time.Time,
) {
	t.Helper()
	if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            endTime,
		NetRevenue:         model.UsdToNanoCents(1),
		SubscriptionMarket: model.SubscriptionMarketStripe,
		TransactionId:      transactionId,
	}); err != nil {
		t.Fatalf("add Stripe renewal: %v", err)
	}
}

// networkRemoveStripeRenewalActive reports whether a synthetic renewal remains
// open after a cancellation attempt.
func networkRemoveStripeRenewalActive(ctx context.Context, networkId server.Id, transactionId string) bool {
	active := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM subscription_renewal
					WHERE network_id = $1
						AND market = $2
						AND transaction_id = $3
						AND end_time > now()
				)
			`,
			networkId,
			model.SubscriptionMarketStripe,
			transactionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&active))
			}
		})
	})
	return active
}

// networkRemoveTestSession builds a synthetic authenticated network session.
func networkRemoveTestSession(ctx context.Context, networkId server.Id, userId server.Id) *session.ClientSession {
	return session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
}

// TestNetworkRemoveStripeCancellationIsFailClosedAndRetryable proves that a
// partial provider failure retains the network and only unfinished work retries.
func TestNetworkRemoveStripeCancellationIsFailClosedAndRetryable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-stripe-remove", userId)
		userSession := networkRemoveTestSession(ctx, networkId, userId)
		fake := newNetworkRemoveStripeFake(t)
		fake.networkPresent = func() bool { return model.GetNetwork(userSession) != nil }

		now := server.NowUtc()
		invoiceA, subscriptionA := "in_synthetic_remove_a", "sub_synthetic_remove_a"
		invoiceB, subscriptionB := "in_synthetic_remove_b", "sub_synthetic_remove_b"
		addNetworkRemoveStripeRenewal(t, ctx, networkId, invoiceA, now.Add(-2*time.Hour), now.Add(2*time.Hour))
		addNetworkRemoveStripeRenewal(t, ctx, networkId, invoiceB, now.Add(-time.Hour), now.Add(3*time.Hour))
		fake.add(invoiceA, subscriptionA, "active", http.StatusOK)
		fake.add(invoiceB, subscriptionB, "active", http.StatusServiceUnavailable, http.StatusOK)

		first, err := NetworkRemove(userSession)
		if err != nil {
			t.Fatalf("first remove returned transport error: %v", err)
		}
		if first == nil || first.Error == nil {
			t.Fatal("first remove succeeded despite a provider cancellation failure")
		}
		if model.GetNetwork(userSession) == nil {
			t.Fatal("provider failure deleted the network")
		}
		if networkRemoveStripeRenewalActive(ctx, networkId, invoiceA) {
			t.Fatal("confirmed cancellation did not close its renewal")
		}
		if !networkRemoveStripeRenewalActive(ctx, networkId, invoiceB) {
			t.Fatal("failed cancellation closed its renewal")
		}

		second, err := NetworkRemove(userSession)
		if err != nil || second == nil || second.Error != nil {
			t.Fatalf("retry remove = %#v, %v", second, err)
		}
		if model.GetNetwork(userSession) != nil {
			t.Fatal("retry did not delete the network after all cancellations succeeded")
		}
		if networkRemoveStripeRenewalActive(ctx, networkId, invoiceB) {
			t.Fatal("retry did not close the remaining renewal")
		}
		if get, deleted := fake.counts(invoiceA, subscriptionA); get != 1 || deleted != 1 {
			t.Fatalf("completed renewal calls = get %d delete %d, want 1/1", get, deleted)
		}
		if get, deleted := fake.counts(invoiceB, subscriptionB); get != 2 || deleted != 2 {
			t.Fatalf("retried renewal calls = get %d delete %d, want 2/2", get, deleted)
		}
		fake.lock.Lock()
		defer fake.lock.Unlock()
		for _, present := range fake.presentOnDelete {
			if !present {
				t.Fatal("provider cancellation ran after local network deletion")
			}
		}
	})
}

// TestNetworkRemoveStripeCanceledStatusCompletesRetryWithoutDelete proves that
// authoritative canceled state completes a retry without another DELETE.
func TestNetworkRemoveStripeCanceledStatusCompletesRetryWithoutDelete(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-stripe-canceled", userId)
		userSession := networkRemoveTestSession(ctx, networkId, userId)
		fake := newNetworkRemoveStripeFake(t)

		now := server.NowUtc()
		invoiceId, subscriptionId := "in_synthetic_canceled", "sub_synthetic_canceled"
		addNetworkRemoveStripeRenewal(t, ctx, networkId, invoiceId, now.Add(-time.Hour), now.Add(time.Hour))
		fake.add(invoiceId, subscriptionId, "canceled")

		result, err := NetworkRemove(userSession)
		if err != nil || result == nil || result.Error != nil {
			t.Fatalf("remove = %#v, %v", result, err)
		}
		if get, deleted := fake.counts(invoiceId, subscriptionId); get != 1 || deleted != 0 {
			t.Fatalf("already-canceled calls = get %d delete %d, want 1/0", get, deleted)
		}
		if model.GetNetwork(userSession) != nil {
			t.Fatal("already-canceled provider state did not permit deletion")
		}
	})
}

// TestNetworkRemoveStripeRequiresExactCancellationConfirmation proves that a
// bare 404 or a mismatched success object cannot authorize local deletion.
func TestNetworkRemoveStripeRequiresExactCancellationConfirmation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fake := newNetworkRemoveStripeFake(t)
		for _, testCase := range []struct {
			name           string
			suffix         string
			deleteStatus   int
			wrongId        bool
			responseStatus string
		}{
			{name: "not found", suffix: "not_found", deleteStatus: http.StatusNotFound},
			{name: "wrong id", suffix: "wrong_id", deleteStatus: http.StatusOK, wrongId: true, responseStatus: "canceled"},
			{name: "not canceled", suffix: "not_canceled", deleteStatus: http.StatusOK, responseStatus: "active"},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-stripe-confirmation-"+testCase.suffix, userId)
			userSession := networkRemoveTestSession(ctx, networkId, userId)

			invoiceId := "in_synthetic_confirmation_" + testCase.suffix
			subscriptionId := "sub_synthetic_confirmation_" + testCase.suffix
			now := server.NowUtc()
			addNetworkRemoveStripeRenewal(t, ctx, networkId, invoiceId, now.Add(-time.Hour), now.Add(time.Hour))
			fake.add(invoiceId, subscriptionId, "active", testCase.deleteStatus)
			responseId := subscriptionId
			if testCase.wrongId {
				responseId = "sub_synthetic_other"
			}
			fake.setDeleteResponse(subscriptionId, responseId, testCase.responseStatus)

			result, err := NetworkRemove(userSession)
			if err != nil || result == nil || result.Error == nil {
				t.Fatalf("%s remove without exact provider confirmation = %#v, %v", testCase.name, result, err)
			}
			if model.GetNetwork(userSession) == nil {
				t.Fatalf("%s provider response authorized deletion", testCase.name)
			}
			if !networkRemoveStripeRenewalActive(ctx, networkId, invoiceId) {
				t.Fatalf("%s provider response closed the local renewal", testCase.name)
			}
		}
	})
}

// TestNetworkRemoveAuthorizesBeforeStripeCancellation proves a non-admin cannot
// invoke provider cancellation by moving it ahead of local deletion.
func TestNetworkRemoveAuthorizesBeforeStripeCancellation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		adminUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-stripe-auth", adminUserId)
		fake := newNetworkRemoveStripeFake(t)

		now := server.NowUtc()
		invoiceId, subscriptionId := "in_synthetic_auth", "sub_synthetic_auth"
		addNetworkRemoveStripeRenewal(t, ctx, networkId, invoiceId, now.Add(-time.Hour), now.Add(time.Hour))
		fake.add(invoiceId, subscriptionId, "active", http.StatusOK)

		result, err := NetworkRemove(networkRemoveTestSession(ctx, networkId, server.NewId()))
		if err == nil || result != nil {
			t.Fatalf("non-admin remove = %#v, %v", result, err)
		}
		if get, deleted := fake.counts(invoiceId, subscriptionId); get != 0 || deleted != 0 {
			t.Fatalf("non-admin provider calls = get %d delete %d, want 0/0", get, deleted)
		}
		if model.GetNetwork(networkRemoveTestSession(ctx, networkId, adminUserId)) == nil {
			t.Fatal("non-admin remove deleted the network")
		}
	})
}

// TestStripeCreditRefusesDeletedNetworkBeforeLedger proves a delayed Stripe
// event cannot consume its idempotency ledger after network deletion.
func TestStripeCreditRefusesDeletedNetworkBeforeLedger(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-stripe-late", userId)
		if success, _ := model.RemoveNetwork(ctx, networkId, &userId); !success {
			t.Fatal("synthetic prerequisite deletion failed")
		}

		invoiceId := "in_synthetic_after_delete"
		now := server.NowUtc()
		credited, err := stripeCreditInvoicePaid(
			ctx,
			networkId,
			invoiceId,
			model.UsdToNanoCents(1),
			now,
			now.Add(time.Hour),
		)
		if credited || !errors.Is(err, model.ErrPaymentNetworkNotFound) {
			t.Fatalf("late credit = %v, %v", credited, err)
		}

		ledgerCount, renewalCount := 0, 0
		server.Db(ctx, func(conn server.PgConn) {
			result, queryErr := conn.Query(
				ctx,
				`
					SELECT
						(SELECT count(*) FROM stripe_invoice WHERE invoice_id = $1),
						(SELECT count(*) FROM subscription_renewal WHERE transaction_id = $1)
				`,
				invoiceId,
			)
			server.WithPgResult(result, queryErr, func() {
				if result.Next() {
					server.Raise(result.Scan(&ledgerCount, &renewalCount))
				}
			})
		})
		if ledgerCount != 0 || renewalCount != 0 {
			t.Fatalf("late credit persisted ledger=%d renewal=%d", ledgerCount, renewalCount)
		}
	})
}
