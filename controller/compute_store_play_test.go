//go:build sidecar_pending

package controller

// Google Play's compute products on the private stack's PostgreSQL and Redis against a fake Android Publisher API
// (BAL-M12-10; DESIGN-COMPUTE §9.5, §9.11, §9.12, D-BAL-8), until CTEST's computetest.FakeStores lands: a consumable
// credited once by its order id and consumed only after; a subscription's renewals credited once each under one
// token; a replacement granting the difference; a voided purchase reversing the lot; a gone network's order refunded;
// a purchase naming no account binding as Pro's do; compute's products never reaching Pro's paths; and a pending
// purchase answering pending. Under -tags sidecar_pending, since the ledger's tables are MG-C1's.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The fake's package.
const testComputePlayPackage = "network.ur.example"

// A fake Android Publisher API for compute: one-time products and their consumes, subscriptions and their
// acknowledgements, and order refunds.
type testComputePlayFake struct {
	stateLock     sync.Mutex
	products      map[string]map[string]any
	subscriptions map[string]map[string]any
	consumes      []string
	acknowledges  []string
	refunds       []string
	// called at each consume with its token, before the fake marks it consumed
	onConsume func(token string)
	server    *httptest.Server
}

func newTestComputePlayFake(t testing.TB) *testComputePlayFake {
	fake := &testComputePlayFake{products: map[string]map[string]any{}, subscriptions: map[string]map[string]any{}}
	writeJson := func(w http.ResponseWriter, status int, object any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(object)
	}
	prefix := "/androidpublisher/v3/applications/{pkg}/"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"purchases/products/{productId}/tokens/{token}", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		purchase, ok := fake.products[r.PathValue("productId")+"/"+r.PathValue("token")]
		if !ok {
			writeJson(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJson(w, http.StatusOK, purchase)
	})
	mux.HandleFunc("POST "+prefix+"purchases/products/{productId}/tokens/{tokenAction}", func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutSuffix(r.PathValue("tokenAction"), ":consume")
		if !ok {
			writeJson(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		fake.stateLock.Lock()
		onConsume := fake.onConsume
		fake.stateLock.Unlock()
		if onConsume != nil {
			onConsume(token)
		}
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		fake.consumes = append(fake.consumes, token)
		if purchase, ok := fake.products[r.PathValue("productId")+"/"+token]; ok {
			purchase["consumptionState"] = 1
		}
		writeJson(w, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("GET "+prefix+"purchases/subscriptionsv2/tokens/{token}", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		sub, ok := fake.subscriptions[r.PathValue("token")]
		if !ok {
			writeJson(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJson(w, http.StatusOK, sub)
	})
	mux.HandleFunc("POST "+prefix+"purchases/subscriptions/{subscriptionId}/tokens/{tokenAction}", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		token, _ := strings.CutSuffix(r.PathValue("tokenAction"), ":acknowledge")
		fake.acknowledges = append(fake.acknowledges, token)
		if sub, ok := fake.subscriptions[token]; ok {
			sub["acknowledgementState"] = "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED"
		}
		writeJson(w, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("POST "+prefix+"orders/{orderAction}", func(w http.ResponseWriter, r *http.Request) {
		fake.stateLock.Lock()
		defer fake.stateLock.Unlock()
		orderId, _ := strings.CutSuffix(r.PathValue("orderAction"), ":refund")
		fake.refunds = append(fake.refunds, orderId)
		writeJson(w, http.StatusOK, map[string]any{})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// A consumable's purchase: purchased unless state says otherwise, named by account where given.
func (self *testComputePlayFake) product(productId string, token string, orderId string, state int, account string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	purchase := map[string]any{"purchaseTimeMillis": fmt.Sprint(server.NowUtc().UnixMilli()), "purchaseState": state, "consumptionState": 0, "orderId": orderId, "purchaseType": 1}
	if account != "" {
		purchase["obfuscatedExternalAccountId"] = account
	}
	self.products[productId+"/"+token] = purchase
}

// A subscription as subscriptionsv2 answers it, its current period paid by orderId, linked to linked where given.
func (self *testComputePlayFake) subscription(token string, productId string, orderId string, start time.Time, expiry time.Time, account string, linked string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sub := map[string]any{
		"startTime": start.Format(time.RFC3339), "subscriptionState": "SUBSCRIPTION_STATE_ACTIVE", "latestOrderId": orderId,
		"acknowledgementState": "ACKNOWLEDGEMENT_STATE_PENDING",
		"lineItems":            []any{map[string]any{"productId": productId, "expiryTime": expiry.Format(time.RFC3339), "latestSuccessfulOrderId": orderId}},
	}
	if account != "" {
		sub["externalAccountIdentifiers"] = map[string]any{"obfuscatedExternalAccountId": account}
	}
	if linked != "" {
		sub["linkedPurchaseToken"] = linked
	}
	self.subscriptions[token] = sub
}

func (self *testComputePlayFake) consumeCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.consumes)
}

func (self *testComputePlayFake) refunded() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.refunds...)
}

// The Play paths over the store env, at the fake.
func newTestComputePlayStore(store *testComputeStoreEnv, fake *testComputePlayFake) *ComputePlayStore {
	return NewComputePlayStore(nil, store.deps, &ComputePlayApi{
		BaseUrl:     func() string { return fake.server.URL },
		PackageName: func() string { return testComputePlayPackage },
		AuthHeader:  func(ctx context.Context, header http.Header) {},
	})
}

// A report from networkId's app.
func testPlayReport(ctx context.Context, play *ComputePlayStore, networkId server.Id, productId string, token string) *VerifyStorePurchaseResult {
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: server.NewId()})
	result, err := play.VerifyPurchase(&VerifyPlayPurchaseArgs{ProductId: productId, PurchaseToken: token}, clientSession)
	if err != nil {
		return &VerifyStorePurchaseResult{Status: "error: " + err.Error()}
	}
	return result
}

// A consumable is credited once by its order id, by the app's report or the RTDN, and consumed only once the credit
// committed.
func TestPlayConsumableCreditedOnceThenConsumed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		fake := newTestComputePlayFake(tb)
		play := newTestComputePlayStore(store, fake)
		networkId := store.network()
		fake.product("compute.credits_25", "token_synthetic_one", "GPA.0000-0000-0000-00001", 0, networkId.String())
		creditedAtConsume := []bool{}
		fake.onConsume = func(token string) {
			creditedAtConsume = append(creditedAtConsume, store.lot(sidecarmodel.ComputeMarketGooglePlay, "GPA.0000-0000-0000-00001") != nil)
		}
		if result := testPlayReport(store.ctx, play, networkId, "compute.credits_25", "token_synthetic_one"); result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("the report: %s", result.Status)
		}
		if handled, err := play.Rtdn(store.ctx, &PlayRtdnMessage{PackageName: testComputePlayPackage, OneTimeProductNotification: &PlayOneTimeProductNotification{
			NotificationType: computePlayOneTimePurchased, PurchaseToken: "token_synthetic_one", Sku: "compute.credits_25",
		}}); !handled || err != nil {
			tb.Fatalf("the notification: %t %v", handled, err)
		}
		if result := testPlayReport(store.ctx, play, networkId, "compute.credits_25", "token_synthetic_one"); result.Status != VerifyPurchaseStatusAlreadyCredited {
			tb.Fatalf("the report again: %s", result.Status)
		}
		if store.credited(networkId) != 25 || fake.consumeCount() != 1 || len(creditedAtConsume) != 1 || !creditedAtConsume[0] {
			tb.Fatalf("credited %v, consumed %d times, credited at consume %v", store.credited(networkId), fake.consumeCount(), creditedAtConsume)
		}
	})
}

// A subscription's periods are credited once each by their own order ids under one purchase token, never by the
// token, which renewals share, and the purchase is acknowledged.
func TestPlayRenewalsCreditedOnceEachUnderOneToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		fake := newTestComputePlayFake(tb)
		play := newTestComputePlayStore(store, fake)
		networkId := store.network()
		start := server.NowUtc()
		rtdn := func(kind int) {
			tb.Helper()
			if handled, err := play.Rtdn(store.ctx, &PlayRtdnMessage{PackageName: testComputePlayPackage, SubscriptionNotification: &PlaySubscriptionNotification{
				NotificationType: kind, PurchaseToken: "token_synthetic_sub", SubscriptionId: "compute.monthly_50",
			}}); !handled || err != nil {
				tb.Fatalf("notification %d: %t %v", kind, handled, err)
			}
		}
		fake.subscription("token_synthetic_sub", "compute.monthly_50", "GPA.1111-1111-1111-11111", start, start.AddDate(0, 1, 0), networkId.String(), "")
		rtdn(computePlaySubscriptionPurchased)
		fake.subscription("token_synthetic_sub", "compute.monthly_50", "GPA.1111-1111-1111-11111..0", start, start.AddDate(0, 2, 0), networkId.String(), "")
		rtdn(computePlaySubscriptionRenewed)
		rtdn(computePlaySubscriptionRenewed)
		if result := testPlayReport(store.ctx, play, networkId, "compute.monthly_50", "token_synthetic_sub"); result.Status != VerifyPurchaseStatusAlreadyCredited {
			tb.Fatalf("the report of a credited period: %s", result.Status)
		}
		if store.credited(networkId) != 100 || store.lot(sidecarmodel.ComputeMarketGooglePlay, "GPA.1111-1111-1111-11111..0") == nil ||
			store.lot(sidecarmodel.ComputeMarketGooglePlay, "token_synthetic_sub") != nil {
			tb.Fatalf("two periods credited %v", store.credited(networkId))
		}
	})
}

// A replacement grants the new plan's credits less the old plan's for the part of its period Play gives back, so four
// changes in a sitting grant about the largest plan.
func TestPlayReplacementGrantsTheDifference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		fake := newTestComputePlayFake(tb)
		play := newTestComputePlayStore(store, fake)
		networkId := store.network()
		start := server.NowUtc()
		linked := ""
		for i, credits := range []int64{10, 25, 50, 100} {
			token := fmt.Sprintf("token_synthetic_plan_%d", i)
			changed := start.Add(time.Duration(i) * time.Minute)
			fake.subscription(token, sidecar.ComputeStoreProductId(true, credits), fmt.Sprintf("GPA.2222-2222-2222-2222%d", i), changed, changed.AddDate(0, 1, 0), networkId.String(), linked)
			if result := testPlayReport(store.ctx, play, networkId, sidecar.ComputeStoreProductId(true, credits), token); result.Status != VerifyPurchaseStatusCredited {
				tb.Fatalf("change %d: %s", i, result.Status)
			}
			linked = token
		}
		if credited := store.credited(networkId); credited < 99.9 || 100.1 < credited {
			tb.Fatalf("four replacements in a sitting credited %v credits, not about the largest plan's 100", credited)
		}
	})
}

// A voided purchase reverses its order's lot; a voided order compute never credited is left to Pro's paths.
func TestPlayVoidedPurchaseReversesTheLot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		fake := newTestComputePlayFake(tb)
		play := newTestComputePlayStore(store, fake)
		networkId := store.network()
		fake.product("compute.credits_50", "token_synthetic_voided", "GPA.3333-3333-3333-33333", 0, networkId.String())
		if result := testPlayReport(store.ctx, play, networkId, "compute.credits_50", "token_synthetic_voided"); result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("the report: %s", result.Status)
		}
		handled, err := play.Rtdn(store.ctx, &PlayRtdnMessage{PackageName: testComputePlayPackage, VoidedPurchaseNotification: &PlayVoidedPurchaseNotification{
			PurchaseToken: "token_synthetic_voided", OrderId: "GPA.3333-3333-3333-33333", ProductType: 2, RefundType: 1,
		}})
		if !handled || err != nil {
			tb.Fatalf("the voided purchase: %t %v", handled, err)
		}
		if lot := store.lot(sidecarmodel.ComputeMarketGooglePlay, "GPA.3333-3333-3333-33333"); lot == nil || lot.ReversedTime == nil {
			tb.Fatalf("the lot stands: %+v", lot)
		}
		handled, err = play.Rtdn(store.ctx, &PlayRtdnMessage{PackageName: testComputePlayPackage, VoidedPurchaseNotification: &PlayVoidedPurchaseNotification{
			PurchaseToken: "token_synthetic_pro", OrderId: "GPA.9999-9999-9999-99999", ProductType: 1, RefundType: 1,
		}})
		if handled || err != nil {
			tb.Fatalf("a voided order compute never credited: %t %v", handled, err)
		}
	})
}

// A purchase naming a network that is gone credits nothing, and its order is refunded through Play.
func TestPlayGoneNetworkRefunded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		fake := newTestComputePlayFake(tb)
		play := newTestComputePlayStore(store, fake)
		gone := server.NewId()
		fake.product("compute.credits_10", "token_synthetic_gone", "GPA.4444-4444-4444-44444", 0, gone.String())
		handled, err := play.Rtdn(store.ctx, &PlayRtdnMessage{PackageName: testComputePlayPackage, OneTimeProductNotification: &PlayOneTimeProductNotification{
			NotificationType: computePlayOneTimePurchased, PurchaseToken: "token_synthetic_gone", Sku: "compute.credits_10",
		}})
		if !handled || err != nil {
			tb.Fatalf("a gone network: %t %v", handled, err)
		}
		if refunds := fake.refunded(); len(refunds) != 1 || refunds[0] != "GPA.4444-4444-4444-44444" {
			tb.Fatalf("refunds %v", refunds)
		}
		if store.lot(sidecarmodel.ComputeMarketGooglePlay, "GPA.4444-4444-4444-44444") != nil || fake.consumeCount() != 0 {
			tb.Fatal("a gone network was credited, or its purchase consumed")
		}
	})
}

// A purchase naming no account binds as Pro's do: to the network whose app reports it first, its RTDN before then
// waiting, another network's report wrong_network.
func TestPlayNoAccountBindsAsPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		fake := newTestComputePlayFake(tb)
		play := newTestComputePlayStore(store, fake)
		reporter := store.network()
		other := store.network()
		fake.product("compute.credits_10", "token_synthetic_unbound", "GPA.5555-5555-5555-55555", 0, "")
		notification := &PlayRtdnMessage{PackageName: testComputePlayPackage, OneTimeProductNotification: &PlayOneTimeProductNotification{
			NotificationType: computePlayOneTimePurchased, PurchaseToken: "token_synthetic_unbound", Sku: "compute.credits_10",
		}}
		if handled, err := play.Rtdn(store.ctx, notification); !handled || err == nil {
			tb.Fatalf("an unbound notification: %t %v", handled, err)
		}
		if result := testPlayReport(store.ctx, play, reporter, "compute.credits_10", "token_synthetic_unbound"); result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("the reporter: %s", result.Status)
		}
		if store.count(`SELECT count(*) FROM play_purchase_binding WHERE purchase_token = $1 AND network_id = $2`, "token_synthetic_unbound", reporter) != 1 {
			tb.Fatal("the report bound nothing")
		}
		if result := testPlayReport(store.ctx, play, other, "compute.credits_10", "token_synthetic_unbound"); result.Status != VerifyPurchaseStatusWrongNetwork {
			tb.Fatalf("another network's report: %s", result.Status)
		}
		if handled, err := play.Rtdn(store.ctx, notification); !handled || err != nil {
			tb.Fatalf("the bound notification: %t %v", handled, err)
		}
		if store.credited(reporter) != 10 || store.credited(other) != 0 {
			tb.Fatalf("credited %v and %v", store.credited(reporter), store.credited(other))
		}
	})
}

// Compute's products, through the routes Pro's purchases take, reach no Pro path: no renewal or Pro balance; and a
// Pro subscription's message is none of compute's.
func TestPlayComputeNeverGrantsPro(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		fake := newTestComputePlayFake(tb)
		play := newTestComputePlayStore(store, fake)
		previousProcess, previousPackage, previousRate := computePlayStoreProcess, playPackageNameFunc, playVerifyRateLimitFunc
		computePlayStoreProcess = func() *ComputePlayStore { return play }
		playPackageNameFunc = func() string { return testComputePlayPackage }
		playVerifyRateLimitFunc = func(clientSession *session.ClientSession) error { return nil }
		defer func() {
			computePlayStoreProcess, playPackageNameFunc, playVerifyRateLimitFunc = previousProcess, previousPackage, previousRate
		}()
		networkId := store.network()
		clientSession := session.Testing_CreateClientSession(store.ctx, &session.ByJwt{NetworkId: networkId, UserId: server.NewId()})
		fake.product("compute.credits_25", "token_synthetic_routed", "GPA.6666-6666-6666-66666", 0, networkId.String())
		result, err := VerifyPlayPurchase(&VerifyPlayPurchaseArgs{ProductId: "compute.credits_25", PurchaseToken: "token_synthetic_routed"}, clientSession)
		if err != nil || result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("the routed report: %+v %v", result, err)
		}
		start := server.NowUtc()
		fake.subscription("token_synthetic_routed_sub", "compute.monthly_10", "GPA.7777-7777-7777-77777", start, start.AddDate(0, 1, 0), networkId.String(), "")
		message, _ := json.Marshal(&PlayRtdnMessage{PackageName: testComputePlayPackage, SubscriptionNotification: &PlaySubscriptionNotification{
			NotificationType: computePlaySubscriptionPurchased, PurchaseToken: "token_synthetic_routed_sub", SubscriptionId: "compute.monthly_10",
		}})
		if _, err := PlayWebhook(&PlayWebhookArgs{Message: &PlayWebhookMessage{Data: base64.StdEncoding.EncodeToString(message)}}, clientSession); err != nil {
			tb.Fatalf("the routed RTDN: %v", err)
		}
		if store.count(`SELECT count(*) FROM subscription_renewal WHERE network_id = $1`, networkId) != 0 ||
			store.count(`SELECT count(*) FROM transfer_balance WHERE network_id = $1`, networkId) != 0 {
			tb.Fatal("a compute product reached Pro's path")
		}
		if store.credited(networkId) != 35 {
			tb.Fatalf("compute credited %v", store.credited(networkId))
		}
		handled, err := play.Rtdn(store.ctx, &PlayRtdnMessage{PackageName: testComputePlayPackage, SubscriptionNotification: &PlaySubscriptionNotification{
			NotificationType: computePlaySubscriptionPurchased, PurchaseToken: "token_synthetic_pro", SubscriptionId: "supporter_monthly_26",
		}})
		if handled || err != nil {
			tb.Fatalf("a Pro subscription's message: %t %v", handled, err)
		}
	})
}

// A pending purchase answers pending, credits nothing and is not consumed; once purchased, the report credits it.
func TestPlayPendingAnswersPending(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		store := newTestComputeStoreEnv(tb)
		fake := newTestComputePlayFake(tb)
		play := newTestComputePlayStore(store, fake)
		networkId := store.network()
		fake.product("compute.credits_100", "token_synthetic_pending", "GPA.8888-8888-8888-88888", computePlayPending, networkId.String())
		if result := testPlayReport(store.ctx, play, networkId, "compute.credits_100", "token_synthetic_pending"); result.Status != VerifyPurchaseStatusPending {
			tb.Fatalf("a pending purchase: %s", result.Status)
		}
		if store.credited(networkId) != 0 || fake.consumeCount() != 0 {
			tb.Fatal("a pending purchase was credited or consumed")
		}
		fake.product("compute.credits_100", "token_synthetic_pending", "GPA.8888-8888-8888-88888", computePlayPurchased, networkId.String())
		if result := testPlayReport(store.ctx, play, networkId, "compute.credits_100", "token_synthetic_pending"); result.Status != VerifyPurchaseStatusCredited {
			tb.Fatalf("once purchased: %s", result.Status)
		}
	})
}
